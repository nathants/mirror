package repaircopies

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Progress changes either one file's state, the final verification time, or the
// operation error. It never changes scan evidence. Exactly one kind is required.
type Progress struct {
	Repair   *int       `json:"repair,omitempty"`
	State    string     `json:"state,omitempty"`
	Error    string     `json:"error,omitempty"`
	FinishAt *time.Time `json:"finish_at,omitempty"`
}

type journalRecord struct {
	Sequence int `json:"sequence"`
	Progress
}

// Journal retains the append descriptor and the last successfully synced state.
// Its caller must hold the operation lock. After a write/sync failure, no more
// records are accepted: the tail may be incomplete or durability uncertain.
type Journal struct {
	path     string
	file     *os.File
	log      Log
	sequence int
	size     int64
	failed   error
}

// OpenJournal replays durable records and atomically compacts them once, before
// further work. A crash leaves either the previous stream or the complete new
// snapshot. An incomplete final record is discarded, never interpreted as success.
func OpenJournal(path string) (*Journal, error) {
	file, err := openLog(path, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	log, compact, err := readLog(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	j := &Journal{path: path, file: file, log: log, size: info.Size()}
	if err := j.check(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if compact {
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := Save(path, log); err != nil {
			return nil, fmt.Errorf("compact repair journal: %w", err)
		}
		file, err = openLog(path, os.O_RDWR|os.O_APPEND)
		if err != nil {
			return nil, err
		}
		info, err = file.Stat()
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		j.file, j.size = file, info.Size()
	}
	return j, nil
}

func (j *Journal) Close() error { return j.file.Close() }

func (j *Journal) check() error {
	if j.failed != nil {
		return fmt.Errorf("repair journal is uncertain after an earlier failure: %w", j.failed)
	}
	opened, err := j.file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(j.path)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, current) || opened.Size() != j.size {
		return errors.New("repair journal changed outside this operation")
	}
	return nil
}

func (j *Journal) Append(progress Progress) error {
	if err := j.check(); err != nil {
		return err
	}
	if err := validateProgress(j.log, progress); err != nil {
		return err
	}
	record := journalRecord{Sequence: j.sequence + 1, Progress: progress}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := j.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = j.file.Sync()
	}
	if err != nil {
		j.failed = err
		return fmt.Errorf("append repair progress (last checkpoint may be uncertain): %w", err)
	}
	j.size += int64(n)
	j.sequence = record.Sequence
	applyProgress(&j.log, progress)
	return nil
}

func validateProgress(log Log, p Progress) error {
	switch {
	case p.Repair != nil:
		if p.FinishAt != nil || log.Status != StatusPending || *p.Repair < 0 || *p.Repair >= len(log.Repairs) {
			return errors.New("invalid repair progress target")
		}
		before := log.Repairs[*p.Repair].State
		switch p.State {
		case RepairStarted:
			if before == RepairCompleted || p.Error != "" {
				return errors.New("cannot restart a completed repair or start with an error")
			}
		case RepairCompleted, RepairFailed:
			if before != RepairStarted || (p.State == RepairFailed) != (p.Error != "") {
				return errors.New("repair result requires a start and a consistent error")
			}
		default:
			return errors.New("invalid repair progress state")
		}
	case p.FinishAt != nil:
		if p.State != "" || p.Error != "" || log.Status != StatusPending || p.FinishAt.Location() != time.UTC || p.FinishAt.Before(log.ScannedAt) {
			return errors.New("invalid repair finish record")
		}
		for _, repair := range log.Repairs {
			if repair.State != RepairCompleted {
				return errors.New("cannot finish with incomplete repairs")
			}
		}
	case p.Error != "":
		if p.State != "" {
			return errors.New("operation error contains a repair state")
		}
	default:
		return errors.New("empty repair progress record")
	}
	return nil
}

// Called only after record validation and, for live work, successful Sync.
func applyProgress(log *Log, p Progress) {
	switch {
	case p.Repair != nil:
		log.Repairs[*p.Repair].State = p.State
		log.Repairs[*p.Repair].Error = p.Error
		log.Error = ""
	case p.FinishAt != nil:
		log.Status, log.CompletedAt, log.Error = StatusCompleted, *p.FinishAt, ""
		if len(log.Unresolved) > 0 {
			log.Status, log.CompletedAt = StatusUnresolved, time.Time{}
		}
	default:
		log.Error = p.Error
		if log.Status == StatusCompleted {
			log.Status, log.CompletedAt = StatusPending, time.Time{}
		}
	}
}

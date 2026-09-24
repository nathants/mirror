package commandlog

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"
)

func TestOutputAndArgumentsRoundTripArbitraryBytes(t *testing.T) {
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	for _, test := range []struct {
		name   string
		args   []string
		chunks [][]byte
	}{
		{"UTF-8", []string{"ensure-copies", "--preview"}, [][]byte{[]byte("path: café/日本語\n"), []byte("prompt: ")}},
		{"arbitrary bytes", []string{"command", "", "path-\xff"}, [][]byte{allBytes, []byte("after invalid bytes\n")}},
		{"split UTF-8 rune", []string{"valid-Ω"}, [][]byte{{0xce}, {0xa9}, []byte("\n")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			log, err := Open(t.TempDir(), os.Geteuid(), os.Getegid(), test.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			writer := log.Writer("stdout", &output)
			for _, chunk := range test.chunks {
				if n, err := writer.Write(chunk); n != len(chunk) || err != nil {
					t.Fatalf("write = %d, %v", n, err)
				}
			}
			log.Close(0)
			want := bytes.Join(test.chunks, nil)
			if !bytes.Equal(output.Bytes(), want) {
				t.Fatal("changed terminal bytes")
			}
			data, err := os.ReadFile(log.file.(*os.File).Name())
			if err != nil {
				t.Fatal(err)
			}
			// Decode the on-disk format independently of the logger's event type.
			var got bytes.Buffer
			var args []string
			decoder := json.NewDecoder(bytes.NewReader(data))
			for {
				var value struct {
					Kind       string
					Text       string
					Bytes      []byte `json:"bytes_base64"`
					Args       []string
					ArgsBase64 [][]byte `json:"args_base64"`
				}
				if err := decoder.Decode(&value); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				switch value.Kind {
				case "start":
					args = value.Args
					if value.ArgsBase64 != nil {
						if value.Args != nil {
							t.Fatal("ambiguous argument encoding")
						}
						for _, arg := range value.ArgsBase64 {
							args = append(args, string(arg))
						}
					}
				case "output":
					if value.Bytes != nil {
						if value.Text != "" {
							t.Fatal("ambiguous output encoding")
						}
						got.Write(value.Bytes)
					} else {
						got.WriteString(value.Text)
					}
				}
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("log changed output bytes:\n got %x\nwant %x", got.Bytes(), want)
			}
			if !reflect.DeepEqual(args, test.args) {
				t.Errorf("log changed argument bytes: got %q, want %q", args, test.args)
			}
		})
	}
}

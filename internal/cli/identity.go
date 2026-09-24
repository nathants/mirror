package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
)

func requireSudoCaller(command string, platform platform) (callerIdentity, error) {
	if platform.effectiveUID == nil || platform.lookupEnv == nil || platform.lookupUserID == nil {
		return callerIdentity{}, errors.New("operating-system identity functions are unavailable")
	}
	if platform.effectiveUID() != 0 {
		return callerIdentity{}, fmt.Errorf("%s requires root privileges; run: sudo mirror %s", command, command)
	}

	uidText, err := requiredEnvironment(platform, "SUDO_UID")
	if err != nil {
		return callerIdentity{}, fmt.Errorf("cannot identify the sudo caller: %w", err)
	}
	gidText, err := requiredEnvironment(platform, "SUDO_GID")
	if err != nil {
		return callerIdentity{}, fmt.Errorf("cannot identify the sudo caller: %w", err)
	}
	username, err := requiredEnvironment(platform, "SUDO_USER")
	if err != nil {
		return callerIdentity{}, fmt.Errorf("cannot identify the sudo caller: %w", err)
	}
	uid, err := parseID("SUDO_UID", uidText)
	if err != nil {
		return callerIdentity{}, err
	}
	gid, err := parseID("SUDO_GID", gidText)
	if err != nil {
		return callerIdentity{}, err
	}

	account, err := platform.lookupUserID(uidText)
	if err != nil {
		return callerIdentity{}, fmt.Errorf("look up sudo caller UID %s: %w", uidText, err)
	}
	if account == nil {
		return callerIdentity{}, fmt.Errorf("look up sudo caller UID %s: no account returned", uidText)
	}
	accountUID, err := parseID("passwd UID", account.Uid)
	if err != nil {
		return callerIdentity{}, err
	}
	accountGID, err := parseID("passwd GID", account.Gid)
	if err != nil {
		return callerIdentity{}, err
	}
	if accountUID != uid {
		return callerIdentity{}, fmt.Errorf("sudo UID %d does not match passwd UID %d", uid, accountUID)
	}
	if accountGID != gid {
		return callerIdentity{}, fmt.Errorf("sudo GID %d does not match passwd GID %d", gid, accountGID)
	}
	if account.Username != username {
		return callerIdentity{}, fmt.Errorf("sudo user %q does not match passwd user %q", username, account.Username)
	}
	if !filepath.IsAbs(account.HomeDir) || filepath.Clean(account.HomeDir) != account.HomeDir || account.HomeDir == string(filepath.Separator) {
		return callerIdentity{}, fmt.Errorf("sudo caller home directory is not a clean non-root absolute path: %q", account.HomeDir)
	}
	return callerIdentity{HomeDir: account.HomeDir, UID: uid, GID: gid}, nil
}

// Logging also covers help and parser failures, which do not require sudo. An
// elevated process with any sudo metadata must validate it rather than falling
// back to a potentially unrelated HOME or trusting a bare SUDO_UID.
func loggingCaller(platform platform) (callerIdentity, error) {
	if platform.effectiveUID == nil || platform.effectiveGID == nil || platform.homeDir == nil || platform.lookupEnv == nil {
		return callerIdentity{}, errors.New("operating-system identity functions are unavailable")
	}
	if platform.effectiveUID() == 0 {
		for _, name := range []string{"SUDO_UID", "SUDO_GID", "SUDO_USER"} {
			if _, found := platform.lookupEnv(name); found {
				return requireSudoCaller("logging", platform)
			}
		}
	}
	home, err := platform.homeDir()
	if err != nil {
		return callerIdentity{}, err
	}
	return callerIdentity{HomeDir: home, UID: platform.effectiveUID(), GID: platform.effectiveGID()}, nil
}

func requiredEnvironment(platform platform, name string) (string, error) {
	value, found := platform.lookupEnv(name)
	if !found || value == "" {
		return "", fmt.Errorf("%s is missing; invoke this command through sudo", name)
	}
	return value, nil
}

func parseID(name, value string) (int, error) {
	parsed, err := strconv.ParseUint(value, 10, strconv.IntSize)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %q", name, value)
	}
	return int(parsed), nil
}

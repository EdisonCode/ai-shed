package runlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BumpsDir holds one file per issue the owner put first in line, named for
// the issue's number. The file's content is the worker the issue goes to, or
// empty to leave it with the workers whose queue it is in.
const BumpsDir = "bumps"

// Bumps returns the issues the owner put first in line, by number, each with
// the worker it goes to ("" for the workers whose queue it is in). A missing
// directory is no bumps.
func Bumps(dir string) (map[int]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, BumpsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read bumps: %w", err)
	}
	bumps := map[int]string{}
	for _, e := range entries {
		// A file that is still being written has another name.
		issue, err := strconv.Atoi(e.Name())
		if err != nil || e.IsDir() {
			continue
		}
		worker, err := os.ReadFile(filepath.Join(dir, BumpsDir, e.Name()))
		if err != nil {
			continue
		}
		bumps[issue] = strings.TrimSpace(string(worker))
	}
	return bumps, nil
}

// RemoveBump puts the issue back in its usual place. An issue that was not
// bumped is no error.
func RemoveBump(dir string, issue int) error {
	err := os.Remove(filepath.Join(dir, BumpsDir, strconv.Itoa(issue)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the bump of #%d: %w", issue, err)
	}
	return nil
}

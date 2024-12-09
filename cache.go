// Dutifully borrowed and modified from https://github.com/burrowers/garble/blob/master/hash.go
// Big thanks to mvdan and lu4p.

package goinject

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	hasher = sha256.New()
)

const buildIDHashLength = 15

func alterToolVersion(tool string, args []string) error {
	line, err := execCmd(tool, args...)
	if err != nil {
		return fmt.Errorf("calling %s %q: %w", tool, args, err)
	}

	f := strings.Fields(line)
	_, toolName := filepath.Split(tool)
	if len(f) < 3 || f[0] != toolName || f[1] != "version" && !strings.HasPrefix(f[len(f)-1], "buildID=") {
		fmt.Print(line)
		return nil
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("retrieving executable path: %w", err)
	}

	packageID := []byte(line)
	contentID, err := addToolToHash(execPath, packageID)
	if err != nil {
		return fmt.Errorf("adding tool id to hash: %w", err)
	}

	// The part of the build ID that matters is the last, since it's the
	// "content ID" which is used to work out whether there is a need to redo
	// the action (build) or not. Since cmd/go parses the last word in the
	// output as "buildID=...", we simply add "+toolName buildID=_/_/_/${hash}".
	// The slashes let us imitate a full binary build ID, but we assume that
	// the other hashes such as the action ID are not necessary, since the
	// only reader here is cmd/go and it only consumes the content ID.
	fmt.Printf("%s +%s buildID=_/_/_/%s\n", line, toolName, encodeBuildIDHash(contentID))
	return nil
}

func addToolToHash(execPath string, inputHash []byte) ([sha256.Size]byte, error) {
	// Join the two content IDs together into a single base64-encoded sha256
	// sum. This includes the original tool's content ID, and tool's own
	// content ID.
	hasher.Reset()
	hasher.Write(inputHash)

	toolID, err := buildidOf(execPath)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("retrieving buildid of %s: %w", execPath, err)
	}

	hasher.Write([]byte(toolID))

	// addToolToHash returns the sum buffer, so we need a new copy.
	// Otherwise the next use of the global sumBuffer would conflict.
	var sumBuffer [sha256.Size]byte
	hasher.Sum(sumBuffer[:0])
	return sumBuffer, nil
}

func buildidOf(path string) (string, error) {
	return execCmd("go", "tool", "buildid", path)
}

func execCmd(name string, arg ...string) (string, error) {
	cmd := exec.Command(name, arg...)
	out, err := cmd.Output()
	if err != nil {
		if err, _ := err.(*exec.ExitError); err != nil {
			return "", fmt.Errorf("%v: %s", err, err.Stderr)
		}
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

func encodeBuildIDHash(h [sha256.Size]byte) string {
	return base64.RawURLEncoding.EncodeToString(h[:buildIDHashLength])
}

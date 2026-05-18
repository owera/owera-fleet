// exec.go isolates the os/exec dependency so the scenarios can run
// without a child process in tests.
package usecasetests

import (
	"bytes"
	"os/exec"
)

// realExecCmd runs `name args...`, returning combined stdout+stderr.
// Used by S3's git-stat call. Returns the partial output even on error
// so callers can include it in failure messages.
func realExecCmd(name string, args ...string) (string, error) {
	var buf bytes.Buffer
	c := exec.Command(name, args...)
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	return buf.String(), err
}

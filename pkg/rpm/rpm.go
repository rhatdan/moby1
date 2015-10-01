package rpm

import (
	"os/exec"
	"strings"
)

// Return package version for the specified package or executable path
func Version(name string) (string, error) {
	var (
		err    error
		out    []byte
		option = "-q"
	)
	if name[0] == '/' {
		option = "-qf"
	}
	out, err = exec.Command("/usr/bin/rpm", option, name).Output()
	return strings.Trim(string(out), "\n"), err
}

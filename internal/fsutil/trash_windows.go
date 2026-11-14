//go:build windows

package fsutil

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Trash sends p to the Recycle Bin through the .NET FileSystem API, which
// needs no cgo and is present on every supported Windows release.
func Trash(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	method := "DeleteFile"
	if fi.IsDir() {
		method = "DeleteDirectory"
	}
	quoted := "'" + strings.ReplaceAll(p, "'", "''") + "'"
	script := "Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.FileIO.FileSystem]::" + method +
		"(" + quoted + ", 'OnlyErrorDialogs', 'SendToRecycleBin')"
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("recycle bin: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

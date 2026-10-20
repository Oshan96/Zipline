package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
)

// cliStdin is where --password-stdin reads the password. A password given with --password shows
// in the process list for everyone on the computer to read; one read from standard input doesn't.
var cliStdin io.Reader = os.Stdin

// readPasswordLine reads the password: the first line, without its line end.
func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("--password-stdin: there was no password on standard input")
	}
	return line, nil
}

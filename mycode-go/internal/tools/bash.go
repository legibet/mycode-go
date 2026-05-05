package tools

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// trimTailLines bounds tailLines to the most recent DefaultMaxLines entries.
func trimTailLines(lines []string) []string {
	if len(lines) <= DefaultMaxLines {
		return lines
	}
	return slices.Clone(lines[len(lines)-DefaultMaxLines:])
}

// Bash runs a shell command and streams output.
func (e *Executor) Bash(toolCallID, command string, timeoutSeconds int, onOutput OutputCallback) Result {
	timeout := int(BashTimeout / time.Second)
	if timeoutSeconds > 0 {
		timeout = timeoutSeconds
	}

	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = e.cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	reader, writer := io.Pipe()
	cmd.Stdout = writer
	cmd.Stderr = writer

	if err := cmd.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return errorResult("error: " + err.Error())
	}
	waited := false
	killed := false
	e.trackCmd(cmd)
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
		e.untrackCmd(cmd)
		if !waited && !killed {
			killCmdTree(cmd)
		}
	}()

	lines := make(chan string, 256)
	readerErrors := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		for scanner.Scan() {
			lines <- strings.TrimRight(scanner.Text(), "\n")
		}
		if err := scanner.Err(); err != nil {
			readerErrors <- err
		}
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
		_ = writer.Close()
	}()

	logPath := filepath.Join(e.toolOutputDir, "bash-"+toolCallID+".log")
	keptLines := []string{}
	keptBytes := 0
	totalLineCount := 0
	tailLines := []string{}
	var logFile *os.File
	var savedOutputPath string
	doneReading := false
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)

	for !doneReading {
		if time.Now().After(deadline) {
			killCmdTree(cmd)
			killed = true
			if logFile != nil {
				_ = logFile.Close()
			}
			return Result{Output: fmt.Sprintf("error: timeout after %ds", timeout), IsError: true}
		}

		select {
		case err := <-readerErrors:
			if logFile != nil {
				_ = logFile.Close()
			}
			return errorResult("error: " + err.Error())
		case line, ok := <-lines:
			if !ok {
				doneReading = true
				continue
			}
			totalLineCount++
			keptBytes += len(line) + 1

			if logFile == nil {
				keptLines = append(keptLines, line)
				if keptBytes > BashMaxInMemoryBytes {
					if err := os.MkdirAll(e.toolOutputDir, 0o755); err == nil {
						file, fileErr := os.Create(logPath)
						if fileErr == nil {
							logFile = file
							savedOutputPath = logPath
							if len(keptLines) > 0 {
								_, _ = io.WriteString(logFile, strings.Join(keptLines, "\n"))
								_, _ = io.WriteString(logFile, "\n")
								tailLines = append(tailLines, keptLines...)
								tailLines = trimTailLines(tailLines)
							}
							keptLines = nil
						}
					}
				}
			} else {
				tailLines = append(tailLines, line)
				tailLines = trimTailLines(tailLines)
				_, _ = io.WriteString(logFile, line)
				_, _ = io.WriteString(logFile, "\n")
			}

			if onOutput != nil {
				onOutput(line)
			}
		case <-time.After(100 * time.Millisecond):
		}
	}

	waitErr := <-waitDone
	waited = true
	if logFile != nil {
		_ = logFile.Close()
	}
	if waitErr != nil {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			return Result{Output: "error: cancelled", IsError: true}
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() < 0 {
			return Result{Output: "error: cancelled", IsError: true}
		}
	}

	rawOutput := strings.Join(keptLines, "\n")
	if logFile != nil || len(tailLines) > 0 {
		rawOutput = strings.Join(tailLines, "\n")
	}
	output := strings.TrimSpace(rawOutput)
	if output == "" {
		output = "(empty)"
	}
	content, trunc := TruncateText(output, DefaultMaxLines, DefaultMaxBytes, true)
	if savedOutputPath == "" && trunc.Truncated {
		if err := os.MkdirAll(e.toolOutputDir, 0o755); err == nil {
			if err := os.WriteFile(logPath, []byte(rawOutput), 0o644); err == nil {
				savedOutputPath = logPath
			}
		}
	}

	result := content
	wasTruncated := savedOutputPath != "" || trunc.Truncated
	if wasTruncated {
		notice := ""
		if trunc.TruncatedBy == "bytes" {
			if totalLineCount <= 1 {
				notice = fmt.Sprintf("[Truncated: showing last %dKB of output (%dKB limit).", DefaultMaxBytes/1024, DefaultMaxBytes/1024)
			} else {
				notice = fmt.Sprintf("[Truncated: showing tail output (%dKB limit).", DefaultMaxBytes/1024)
			}
		} else {
			notice = fmt.Sprintf("[Truncated: last %d of %d lines.", trunc.OutputLines, totalLineCount)
		}
		if savedOutputPath != "" {
			notice += " Full output: " + savedOutputPath + "]"
		} else {
			notice += "]"
		}
		result += "\n\n" + notice
	}

	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0 {
		result += fmt.Sprintf("\n\n[exit code: %d]", cmd.ProcessState.ExitCode())
	}
	return Result{Output: result}
}

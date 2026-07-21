package pipeline

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

var errGitOutputLimit = errors.New("git output exceeds the configured limit")

type gitRepository struct {
	directory string
}

type blobRequest struct {
	object  string
	maximum int64
}

func (repository gitRepository) run(ctx context.Context, limit int, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = repository.directory
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	var stdout, stderr limitedBuffer
	stdout.limit = limit
	stderr.limit = 64 * 1024
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stdout.exceeded || stderr.exceeded {
			return nil, errGitOutputLimit
		}
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return nil, fmt.Errorf("git %s: %w: %s", arguments[0], err, message)
		}
		return nil, fmt.Errorf("git %s: %w", arguments[0], err)
	}
	if stdout.exceeded {
		return nil, errGitOutputLimit
	}
	return stdout.Bytes(), nil
}

func (repository gitRepository) revision(ctx context.Context, revision string) (Revision, error) {
	commit, err := repository.run(ctx, 128, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return Revision{}, err
	}
	tree, err := repository.run(ctx, 128, "rev-parse", "--verify", revision+"^{tree}")
	if err != nil {
		return Revision{}, err
	}
	value := Revision{Commit: strings.TrimSpace(string(commit)), Tree: strings.TrimSpace(string(tree))}
	if !commitPattern.MatchString(value.Commit) || !commitPattern.MatchString(value.Tree) {
		return Revision{}, errors.New("repository does not use 40-character Git object ids")
	}
	return value, nil
}

func (repository gitRepository) blob(ctx context.Context, object string, maximum int64) ([]byte, error) {
	if !commitPattern.MatchString(object) {
		return nil, errors.New("invalid Git blob id")
	}
	sizeOutput, err := repository.run(ctx, 64, "cat-file", "-s", object)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOutput)), 10, 64)
	if err != nil || size < 0 || size > maximum {
		return nil, fmt.Errorf("Git blob size is outside the %d-byte limit", maximum)
	}
	content, err := repository.run(ctx, int(maximum)+1, "cat-file", "blob", object)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != size {
		return nil, errors.New("Git blob size changed while reading")
	}
	return content, nil
}

// blobs reads an exact object set through one bounded cat-file process. Git's
// batch framing is checked object-by-object before any content is allocated.
func (repository gitRepository) blobs(ctx context.Context, requests []blobRequest, totalMaximum int64) (map[string][]byte, error) {
	result := make(map[string][]byte, len(requests))
	if len(requests) == 0 {
		return result, nil
	}
	if totalMaximum < 0 {
		return nil, errors.New("invalid aggregate Git blob limit")
	}
	var input strings.Builder
	for _, request := range requests {
		if !commitPattern.MatchString(request.object) || request.maximum < 0 {
			return nil, errors.New("invalid Git batch blob request")
		}
		if _, duplicate := result[request.object]; duplicate {
			return nil, errors.New("duplicate Git batch blob request")
		}
		result[request.object] = nil
		input.WriteString(request.object)
		input.WriteByte('\n')
	}

	command := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	command.Dir = repository.directory
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	command.Stdin = strings.NewReader(input.String())
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr limitedBuffer
	stderr.limit = 64 * 1024
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()

	reader := bufio.NewReaderSize(stdout, 256)
	var total int64
	for _, request := range requests {
		header, err := readBatchHeader(reader)
		if err != nil {
			return nil, fmt.Errorf("read Git batch header: %w", err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != request.object || fields[1] != "blob" {
			return nil, fmt.Errorf("Git batch returned an invalid header for %s", request.object)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > request.maximum || total > totalMaximum-size {
			return nil, fmt.Errorf("Git blob %s is outside its configured byte limit", request.object)
		}
		content := make([]byte, int(size))
		if _, err := io.ReadFull(reader, content); err != nil {
			return nil, fmt.Errorf("read Git blob %s: %w", request.object, err)
		}
		delimiter, err := reader.ReadByte()
		if err != nil || delimiter != '\n' {
			return nil, fmt.Errorf("Git batch blob %s has invalid framing", request.object)
		}
		result[request.object] = content
		total += size
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("Git batch returned unexpected trailing output")
		}
		return nil, fmt.Errorf("finish Git batch output: %w", err)
	}
	if err := command.Wait(); err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	finished = true
	return result, nil
}

func readBatchHeader(reader *bufio.Reader) (string, error) {
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	if len(header) < 2 || len(header) > 192 {
		return "", errors.New("Git batch header exceeds its bound")
	}
	return strings.TrimSuffix(string(header), "\n"), nil
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	if buffer.limit < 0 {
		return 0, errGitOutputLimit
	}
	remaining := buffer.limit - buffer.buffer.Len()
	if len(content) > remaining {
		if remaining > 0 {
			_, _ = buffer.buffer.Write(content[:remaining])
		}
		buffer.exceeded = true
		return len(content), errGitOutputLimit
	}
	return buffer.buffer.Write(content)
}

func (buffer *limitedBuffer) Bytes() []byte {
	return append([]byte(nil), buffer.buffer.Bytes()...)
}

func (buffer *limitedBuffer) String() string {
	return buffer.buffer.String()
}

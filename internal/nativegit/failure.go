package nativegit

import "bytes"

// ErrorKind names the class of a failed Git invocation. It is read from the
// last lines of the command's stderr, which otherwise never leaves Run: it
// can contain upstream-provided text and authentication data.
type ErrorKind string

const (
	// FailureOther is a failure that none of the other kinds describe.
	FailureOther ErrorKind = "other"
	// FailureRefLocked is a reference transaction that could not take a
	// lock: a lock is still held, or was left behind by a killed writer.
	FailureRefLocked ErrorKind = "ref_locked"
	// FailureObjectsMissing is a command that found the object store
	// incomplete or damaged.
	FailureObjectsMissing ErrorKind = "objects_missing"
	// FailureOrigin is a fetch the origin did not serve: authentication,
	// a missing repository, a transfer that broke off.
	FailureOrigin ErrorKind = "origin_failed"
)

// Error is a failed Git invocation with its class. The class reaches the log
// as error_reason; the message stays what it was.
type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string {
	if e.Kind == FailureOther {
		return "git operation failed: " + e.Err.Error()
	}
	return "git operation failed (" + string(e.Kind) + "): " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) LogKind() string { return string(e.Kind) }

// stderrTailLimit bounds what is kept of a command's stderr. Git ends with
// its fatal message, so the last few kilobytes are enough to classify.
const stderrTailLimit = 4096

// stderrTail keeps the end of a command's stderr.
type stderrTail struct {
	buffer bytes.Buffer
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.buffer.Write(p)
	if excess := t.buffer.Len() - stderrTailLimit; excess > 0 {
		t.buffer.Next(excess)
	}
	return len(p), nil
}

func (t *stderrTail) bytes() []byte { return t.buffer.Bytes() }

// failureKind names the class of a failure from the end of Git's stderr.
// The patterns are Git's own messages, which are English because the
// command environment pins the locale. A reference lock is the rarest and
// most specific, so it is asked for first; a broken transfer can mention
// objects it did not receive, so the origin is asked about before them.
func failureKind(tail []byte) ErrorKind {
	for _, class := range []struct {
		kind     ErrorKind
		patterns []string
	}{
		{FailureRefLocked, []string{"cannot lock", "lock': File exists"}},
		{FailureOrigin, []string{
			"Could not read from remote repository", "unable to access", "remote end hung up", "remote error",
			"Authentication failed", "Access denied", "access denied", "Repository not found", "requested URL returned error",
			"Could not resolve host", "Failed to connect", "Could not connect", "Connection refused", "Connection was reset", "timed out",
			"does not appear to be a git repository", "RPC failed", "early EOF", "Early EOF", "index-pack failed", "not our ref",
		}},
		{FailureObjectsMissing, []string{
			"did not send all necessary objects", "missing object", "bad object", "nonexistent object", "unable to find",
			"not a valid object", "invalid object", "corrupt", "unable to unpack",
		}},
	} {
		for _, pattern := range class.patterns {
			if bytes.Contains(tail, []byte(pattern)) {
				return class.kind
			}
		}
	}
	return FailureOther
}

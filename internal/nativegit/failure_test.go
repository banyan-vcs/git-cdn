package nativegit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The classes are read from Git's own messages. The table keeps to real
// fatal lines so a change in Git's wording shows up here, and the ordering
// cases hold the precedence the classifier documents: a lock is rarer than
// a broken transfer, and a broken transfer can mention objects it never
// received.
func TestFailureKindReadsGitsOwnMessages(t *testing.T) {
	for _, test := range []struct {
		name string
		tail string
		kind ErrorKind
	}{
		{"reftable stack", "fatal: update_ref failed for ref 'refs/heads/main': cannot lock references\n", FailureRefLocked},
		{"leftover lock file", "error: unable to create directory for '.git/reftable': File exists.\nfatal: cannot lock references\n", FailureRefLocked},
		{"files backend", "fatal: cannot lock ref 'refs/heads/main': Unable to create '/private-path/main.lock': File exists.\n", FailureRefLocked},
		{"dangling pack", "fatal: pack has \n\nunresolved deltas: 12\nfatal: does not appear to be a git repository\n", FailureOrigin},
		{"transfer refused", "fatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\n", FailureOrigin},
		{"authentication", "remote: HTTP Basic: Access denied\nfatal: Authentication failed for 'https://example.test/repo.git/'\n", FailureOrigin},
		{"index-pack", "fatal: early EOF\nfatal: index-pack failed\n", FailureOrigin},
		{"incomplete send", "error: Could not read from remote repository. Did not send all necessary objects\n", FailureOrigin},
		{"bad object", "fatal: bad object private-object\n", FailureObjectsMissing},
		{"missing object", "fatal: missing object for private-path\n", FailureObjectsMissing},
		{"corrupt loose", "error: unable to unpack private-pack header\nfatal: corrupt loose object\n", FailureObjectsMissing},
		{"locale noise first", "warning: some unrelated warning\ndebug: private details\nfatal: cannot lock ref 'refs/heads/main'\n", FailureRefLocked},
		{"both lock and origin", "error: cannot lock ref 'refs/heads/main'\nfatal: unable to access 'https://example.test/repo/': Connection refused\n", FailureRefLocked},
		{"both origin and objects", "fatal: did not send all necessary objects in order to fetch 'refs/heads/main'\nremote: unable to access\n", FailureOrigin},
		{"unclassified", "fatal: private-failure\n", FailureOther},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.kind, failureKind([]byte(test.tail)))
		})
	}
}

// Only the end of stderr is kept, but the kept window still classifies and
// never grows past the limit.
func TestStderrTailKeepsTheEnd(t *testing.T) {
	var tail stderrTail
	for range 1000 {
		_, err := tail.Write([]byte(strings.Repeat("x", 16) + "\n"))
		require.NoError(t, err)
	}
	require.LessOrEqual(t, tail.buffer.Len(), stderrTailLimit)
	_, err := tail.Write([]byte("fatal: cannot lock\n"))
	require.NoError(t, err)
	require.Equal(t, FailureRefLocked, failureKind(tail.bytes()))
}

package protocol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func request(lines ...string) []byte {
	b := Encode("command=fetch\n")
	b = append(b, "0001"...)
	for _, line := range lines {
		b = append(b, Encode(line+"\n")...)
	}
	return append(b, "0000"...)
}

func TestUnsupportedObjectSelectionIsNeverLocal(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, field := range []string{"want-ref refs/heads/CaseSensitive", "shallow " + oid, "deepen 1", "deepen-not refs/heads/Private", "filter blob:none", "packfile-uris https", "future-option value"} {
		f, err := ParseFetch(request("want "+oid, field, "done"))
		require.NoError(t, err, "%q classified as local: %+v %v", field, f, err)
		require.False(t, f.Supported, "%q classified as local: %+v %v", field, f, err)
	}
	f, err := ParseFetch(request("want "+strings.Repeat("a", 64), "done"))
	require.NoError(t, err, "SHA-256 request must be forwarded")
	require.False(t, f.Supported, "SHA-256 request must be forwarded")
}

func TestMalformedMessagesAndRefsAreRejected(t *testing.T) {
	for _, data := range []string{"", "000", "0003", "zzzz", "ffff", "0001", "0012command=fetch\n0001", "0000trailing"} {
		{
			_, err := ParseFetch([]byte(data))
			require.Error(t, err, "malformed input accepted: %q", data)
		}
	}
	oid := strings.Repeat("a", 40)
	for _, name := range []string{"refs/heads/../secret", "refs/heads/.secret", "refs/heads/bad.lock", "refs/heads/x\nupdate refs/heads/main", "refs/heads/x:y"} {
		data := append(Encode(oid+" "+name+"\n"), "0000"...)
		{
			_, err := ParseRefs(data)
			require.Error(t, err, "invalid ref name accepted: %q", name)
		}
	}
}

func TestSnapshotAuthorizationOnlyUsesFreshAdvertisedObjects(t *testing.T) {
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	data := append(Encode(a+" HEAD symref-target:refs/heads/Main\n"), Encode(a+" refs/heads/Main\n")...)
	data = append(data, Encode(b+" refs/tags/V1 peeled:"+a+"\n")...)
	data = append(data, "0000"...)
	s, err := ParseRefs(data)
	require.NoError(t, err)
	require.True(t, s.Allows([]string{a, b}), "snapshot authorization or case changed")
	require.False(t, s.Allows([]string{c}), "snapshot authorization or case changed")
	require.False(t, s.Allows(nil), "snapshot authorization or case changed")
	require.EqualValues(t, "refs/heads/Main", s.Refs[1].Name, "snapshot authorization or case changed")
}

func FuzzParseFetch(f *testing.F) {
	f.Add(request("want "+strings.Repeat("a", 40), "done"))
	f.Add([]byte("0000"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		parsed, err := ParseFetch(data)
		if err == nil && parsed.Supported {
			for _, oid := range append(parsed.Wants, parsed.Haves...) {
				require.True(t, OID(oid), "unsupported OID classified as local")
			}
		}
	})
}

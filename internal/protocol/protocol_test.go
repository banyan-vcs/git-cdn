package protocol

import (
	"fmt"
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
	for _, field := range []string{"want-ref refs/heads/CaseSensitive", "shallow " + oid, "packfile-uris https", "future-option value"} {
		f, err := ParseFetch(request("want "+oid, field, "done"))
		require.NoError(t, err, "%q classified as local: %+v %v", field, f, err)
		require.False(t, f.Supported, "%q classified as local: %+v %v", field, f, err)
	}
	f, err := ParseFetch(request("want "+strings.Repeat("a", 64), "done"))
	require.NoError(t, err, "SHA-256 request must be forwarded")
	require.False(t, f.Supported, "SHA-256 request must be forwarded")
}

// Partial-history directives stay local and cacheable: their directives
// live in the request bytes, so identical shallow or filtered fetches are
// reproducible, while full-fetch reconciliation excludes them.
func TestPartialDirectivesAreLocalButNotFullFetches(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, test := range []struct {
		field  string
		deepen bool
		filter bool
	}{
		{"deepen 1", true, false},
		{"deepen 2147483647", true, false},
		{"deepen-relative", true, false},
		{"deepen-since 2026-01-01T00:00:00Z", true, false},
		{"deepen-not refs/heads/Private", true, false},
		{"filter blob:none", false, true},
		{"filter blob:limit=1024", false, true},
		{"filter tree:0", false, true},
	} {
		f, err := ParseFetch(request("want "+oid, test.field, "done"))
		require.NoError(t, err, "%q failed to parse: %+v %v", test.field, f, err)
		require.True(t, f.Supported, "%q classified as unsupported: %+v", test.field, f)
		require.Equal(t, test.deepen, f.Deepen, "%q deepen flag", test.field)
		require.Equal(t, test.filter, f.Filter, "%q filter flag", test.field)
		require.True(t, f.CacheableFetch(), "%q should be cacheable: %+v", test.field, f)
		require.False(t, f.FullFetch(), "%q must not reconcile refs as a full fetch", test.field)
	}
	for _, field := range []string{"deepen 0", "deepen -1", "deepen x", "filter "} {
		f, err := ParseFetch(request("want "+oid, field, "done"))
		require.NoError(t, err, "%q failed to parse: %+v %v", field, f, err)
		require.False(t, f.Supported, "%q classified as local: %+v", field, f)
	}
}

func TestFullFetchOnlyReconcilesCompleteInitialRequests(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, test := range []struct {
		name string
		args []string
		full bool
	}{
		{"clone", []string{"want " + oid, "done"}, true},
		{"incremental", []string{"want " + oid, "have " + oid, "done"}, false},
		{"negotiation", []string{"want " + oid, "wait-for-done"}, false},
		{"partial", []string{"want " + oid, "filter blob:none", "done"}, false},
		{"shallow", []string{"want " + oid, "deepen 1", "done"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fetch, err := ParseFetch(request(test.args...))
			require.NoError(t, err)
			require.Equal(t, test.full, fetch.FullFetch())
		})
	}
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

// The parse-time fingerprint stands in for the whole ref list inside
// request keys, so anything that changes the list must change it.
func TestSnapshotFingerprintBindsTheRefList(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	build := func(lines ...string) []byte {
		var data []byte
		for _, line := range lines {
			data = append(data, Encode(line+"\n")...)
		}
		return append(data, "0000"...)
	}
	first, err := ParseRefs(build(a+" refs/heads/main", b+" refs/heads/dev"))
	require.NoError(t, err)
	again, err := ParseRefs(build(a+" refs/heads/main", b+" refs/heads/dev"))
	require.NoError(t, err)
	reordered, err := ParseRefs(build(b+" refs/heads/dev", a+" refs/heads/main"))
	require.NoError(t, err)
	withUnborn, err := ParseRefs(build("unborn HEAD symref-target:refs/heads/main", a+" refs/heads/main"))
	require.NoError(t, err)
	shorter, err := ParseRefs(build(a + " refs/heads/main"))
	require.NoError(t, err)
	require.Equal(t, first.Fingerprint(), again.Fingerprint(), "identical ref lists must share one fingerprint")
	require.NotEqual(t, first.Fingerprint(), reordered.Fingerprint(), "ref order is part of the fingerprint")
	require.NotEqual(t, first.Fingerprint(), withUnborn.Fingerprint(), "an unborn ref shifts the fingerprint")
	require.NotEqual(t, first.Fingerprint(), shorter.Fingerprint(), "a missing ref changes the fingerprint")
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

func TestFetchPacketAndObjectCountsAreBounded(t *testing.T) {
	{
		_, err := Decode([]byte(strings.Repeat("0004", MaxCommandPackets+1)))
		require.Error(t, err, "unbounded packet list accepted")
	}
	data := append(Encode("command=fetch\n"), "0001"...)
	data = append(data, []byte(strings.Repeat("0004", MaxCommandPackets))...)
	data = append(data, "0000"...)
	{
		_, err := ParseFetch(data)
		require.Error(t, err, "unbounded command accepted")
	}
	for _, test := range []struct {
		field                   string
		count, wantCap, haveCap int
	}{
		{"want " + strings.Repeat("a", 40), 65, 64, 0},
		{"have " + strings.Repeat("a", 40), 129, 0, 128},
	} {
		lines := make([]string, test.count)
		for i := range lines {
			lines[i] = test.field
		}
		parsed, err := ParseFetch(request(lines...))
		require.NoError(t, err, "oversized object selection did not remain bounded and bypass local execution: %+v %v", parsed, err)
		require.False(t, parsed.Supported, "oversized object selection did not remain bounded and bypass local execution: %+v %v", parsed, err)
		require.Len(t, parsed.Wants, test.wantCap, "oversized object selection did not remain bounded and bypass local execution: %+v %v", parsed, err)
		require.Len(t, parsed.Haves, test.haveCap, "oversized object selection did not remain bounded and bypass local execution: %+v %v", parsed, err)
	}
}

func TestRefsStreamingDoesNotUseCommandPacketLimit(t *testing.T) {
	var data []byte
	for i := range MaxCommandPackets + 1 {
		data = append(data, Encode(strings.Repeat("a", 40)+" refs/heads/branch"+fmt.Sprint(i)+"\n")...)
	}
	data = append(data, "00000002"...)
	snapshot, err := ParseRefs(data)
	require.NoError(t, err, "large refs snapshot: %d %v", len(snapshot.Refs), err)
	require.Len(t, snapshot.Refs, MaxCommandPackets+1, "large refs snapshot: %d %v", len(snapshot.Refs), err)
}

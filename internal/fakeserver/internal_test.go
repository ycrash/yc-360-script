package fakeserver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTheBodyScannerFindsASecretSplitAcrossWrites(t *testing.T) {
	for _, chunks := range [][]string{
		{"xxs3cr", "3t pwyy"},
		{"s", "3", "c", "r", "3", "t", " ", "p", "w"},
		{"s3cr3t", " pw"},
		{"", "s3cr3t pw", ""},
	} {
		m := newMasker("", []string{"other", "s3cr3t pw"})
		scan := m.scanner()

		for _, c := range chunks {
			n, err := scan.Write([]byte(c))
			assert.NoError(t, err)
			assert.Equal(t, len(c), n)
		}

		assert.Equal(t, []int{1}, scan.foundSecrets(), strings.Join(chunks, "|"))
	}
}

func TestTheBodyScannerDoesNotJoinWhatIsApart(t *testing.T) {
	m := newMasker("", []string{"s3cr3t"})
	scan := m.scanner()

	scan.Write([]byte("s3c"))
	scan.Write([]byte(strings.Repeat("x", 100)))
	scan.Write([]byte("r3t"))

	assert.Empty(t, scan.foundSecrets())
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"pgSessions": "pgSessions",
		"../../etc":  "_.._etc",
		"..":         "_",
		"":           "_",
		".hidden":    "hidden",
		"a b/c\\d":   "a_b_c_d",
		"<secret-1>": "_secret-1_",
	} {
		assert.Equal(t, want, safeName(in), in)
	}

	assert.Len(t, safeName(strings.Repeat("a", 300)), 100)
}

func TestExtension(t *testing.T) {
	assert.Equal(t, "zst", extension("zst", []byte{0x28, 0xb5}))
	assert.Equal(t, "gz", extension("../gz", nil), "only letters and digits")
	assert.Equal(t, "txt", extension("", []byte("Full thread dump")))
	assert.Equal(t, "bin", extension("", []byte{0, 1, 2, 3}))
}

func TestMaskRawQueryWithholdsWhatItCannotMask(t *testing.T) {
	m := newMasker("key@1", []string{"pass"})

	assert.Equal(t, "dt=td&apiKey=<api-key>&p=<secret-1>", m.maskRawQuery("dt=td&apiKey=key%401&p=pass"))
	assert.Equal(t, "<withheld: it holds the API key or a secret>", m.maskRawQuery("p=%70ass"))

	// A malformed escape elsewhere in the query does not hide one.
	assert.Equal(t, "<withheld: it holds the API key or a secret>", m.maskRawQuery("p=%70ass&m3apptoken=Billing100%"))
	assert.Equal(t, "m3apptoken=Billing100%", m.maskRawQuery("m3apptoken=Billing100%"))
}

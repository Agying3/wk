package responsehistory

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

func FuzzHistoryImageRoundTrip(f *testing.F) {
	for _, raw := range [][]byte{
		testTurn("中文\\\"", "AP/A/w==", time.Now()),
		[]byte(`{"url":"data:image/png;base64,AB==","next":"data:image/png;base64,AA=="}`),
		[]byte(`{"url":"data:image/png;base64,AAAA\n"}`),
		[]byte(`"data:image/png;base64,AA==""data:image/jpeg;base64,AA=="`),
	} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		packed, refs, images := splitImages(raw)
		got, err := joinImages(packed, refs, images, int64(len(raw)), digest(raw))
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("round trip mismatch: %v", err)
		}
	})
}

func BenchmarkHistoryHotText(b *testing.B) {
	raw := bytes.Repeat([]byte("user: explain this tool result\nassistant: structured JSON output\n"), 1024)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := encodePayload(raw, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistorySplitImage(b *testing.B) {
	raw := testTurn("image", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0, 255, 192, 128}, 256<<10)), time.Now())
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for b.Loop() {
		splitImages(raw)
	}
}

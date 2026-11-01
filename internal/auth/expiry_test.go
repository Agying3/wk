package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestCredentialExpiryUnits(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, expiry := range []int64{0, -1, 1753600000, 1753600000123} {
			raw := fmt.Sprintf(`{"accessToken":"fake","uid":"u","expiresAt":%d}`, expiry)
			if nested {
				raw = fmt.Sprintf(`{"auth":{"accessToken":"fake","expiresAt":%d},"account":{"uid":"u"}}`, expiry)
			}
			a, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			want := expiry
			if expiry > 100_000_000_000 {
				want /= 1000
			}
			if a.Snapshot().ExpiresAt != want || !a.NeedsRefresh(time.Minute) {
				t.Fatalf("nested=%t expiry=%d parsed=%d", nested, expiry, a.Snapshot().ExpiresAt)
			}
		}
	}
}

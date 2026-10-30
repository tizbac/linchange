package autodiscover

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linchange/linchange/config"
)

// An unauthenticated POX request must not be able to make the XML decoder read
// an unbounded body into memory.
func TestPOXRejectsOversizedBody(t *testing.T) {
	a := &Autodiscover{cfg: config.New()}
	body := `<Autodiscover><Request><EMailAddress>alice@example.com` +
		strings.Repeat("x", maxAutodiscoverBody+1) + `</EMailAddress></Request></Autodiscover>`

	req := httptest.NewRequest(http.MethodPost, "/autodiscover.xml", strings.NewReader(body))
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized POX request = %d, want 400", rr.Code)
	}
}

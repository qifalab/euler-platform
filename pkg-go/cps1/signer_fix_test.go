package cps1

import (
	"net/url"
	"testing"
)

// The Authorization header must be found regardless of casing: gateways and
// proxies forward "authorization" lowercase (HTTP/2 always does), and the
// verify endpoint already reads headers case-insensitively.
func TestVerifyAcceptsLowercaseAuthorizationHeader(t *testing.T) {
	nonce := "case-nonce"
	hdrs := baseHeaders("euecs.api.euler.emoera.com", nonce)
	req := Request{
		Method: "GET", Host: "euecs.api.euler.emoera.com", Path: "/",
		Query: url.Values{}, Headers: hdrs, Date: fixedTime,
	}
	signed, err := Sign(req, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	for _, key := range []string{"authorization", "AUTHORIZATION", "Authorization"} {
		vh := make(map[string]string, len(signed.Headers)+1)
		for k, v := range signed.Headers {
			vh[k] = v
		}
		vh[key] = signed.Authorization
		vr := Request{
			Method: "GET", Host: req.Host, Path: req.Path,
			Query: req.Query, Headers: vh, Date: fixedTime,
		}
		if err := Verify(vr, Credentials{AK: testAK, SK: testSK}, testRegion, testService, fixedTime); err != nil {
			t.Errorf("Verify with %q header key: %v", key, err)
		}
	}
}

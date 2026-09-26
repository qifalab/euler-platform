package identity

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// ConfigFromEnv reads only this service's explicitly named configuration. It
// does not load .env files, EID settings, or a default development identity.
func ConfigFromEnv() (Config, error) {
	c := Config{
		Mode:                    os.Getenv("EULER_IDENTITY_MODE"),
		Issuer:                  os.Getenv("EULER_OIDC_ISSUER"),
		ClientID:                os.Getenv("EULER_OIDC_CLIENT_ID"),
		ClientSecret:            os.Getenv("EULER_OIDC_CLIENT_SECRET"),
		PublicOrigin:            os.Getenv("EULER_PUBLIC_URL"),
		RedirectURL:             os.Getenv("EULER_OIDC_REDIRECT_URL"),
		TokenEndpointAuthMethod: os.Getenv("EULER_OIDC_TOKEN_AUTH_METHOD"),
		AuthorizationEndpoint:   os.Getenv("EULER_OAUTH2_AUTHORIZATION_ENDPOINT"),
		TokenEndpoint:           os.Getenv("EULER_OAUTH2_TOKEN_ENDPOINT"),
		UserInfoEndpoint:        os.Getenv("EULER_OAUTH2_USERINFO_ENDPOINT"),
		UserInfoSubjectClaim:    os.Getenv("EULER_OAUTH2_SUBJECT_CLAIM"),
		UserInfoEnvelope:        os.Getenv("EULER_OAUTH2_USERINFO_ENVELOPE"),
	}
	if v := os.Getenv("EULER_ALLOW_INSECURE_LOOPBACK"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return c, errors.New("EULER_ALLOW_INSECURE_LOOPBACK must be true or false")
		}
		c.AllowInsecureLoopback = parsed
	}
	if c.RedirectURL == "" && c.PublicOrigin != "" {
		c.RedirectURL = strings.TrimRight(c.PublicOrigin, "/") + "/auth/callback"
	}
	return c.validate()
}

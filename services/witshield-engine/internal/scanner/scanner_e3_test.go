// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package scanner

import (
	"context"
	"testing"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
)

// The kernel prints each 32-bit word of a socket address in host byte order,
// so 127.0.0.53 (the systemd-resolved stub on Ubuntu) appears as 3500007F.
// Every address in 127.0.0.0/8, ::1 and IPv4-mapped loopback are loopback.
func TestPortsIgnoreWholeLoopbackRange(t *testing.T) {
	tcp := "sl local_address rem_address st\n" +
		" 0: 3500007F:0035 00000000:0000 0A\n" + // 127.0.0.53:53
		" 1: 3600007F:0035 00000000:0000 0A\n" + // 127.0.0.54:53
		" 2: 0101007F:0277 00000000:0000 0A\n" + // 127.0.1.1:631
		" 3: 0100007F:1538 00000000:0000 0A\n" + // 127.0.0.1:5432
		" 4: 00000000:1F90 00000000:0000 0A\n" + // 0.0.0.0:8080
		" 5: 0A00000A:0050 00000000:0000 0A\n" // 10.0.0.10:80
	tcp6 := "sl local_address rem_address st\n" +
		" 0: 00000000000000000000000001000000:0019 00000000000000000000000000000000:0000 0A\n" + // [::1]:25
		" 1: 0000000000000000FFFF00000100007F:1A0A 00000000000000000000000000000000:0000 0A\n" + // [::ffff:127.0.0.1]:6666
		" 2: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A\n" // [::]:443
	h := fakeHost{files: map[string][]byte{"/proc/net/tcp": []byte(tcp), "/proc/net/tcp6": []byte(tcp6)}}
	got, err := checkPorts(context.Background(), h)
	if err != nil || len(got) != 1 || got[0].Evidence != "Non-loopback TCP ports: 80, 443, 8080" {
		t.Fatalf("loopback listeners were reported as reachable: err=%v findings=%#v", err, got)
	}
}

func TestPortsOnlyLoopbackListenersProduceNoFinding(t *testing.T) {
	tcp := "sl local_address rem_address st\n 0: 3500007F:0035 00000000:0000 0A\n"
	h := fakeHost{files: map[string][]byte{"/proc/net/tcp": []byte(tcp)}}
	got, err := checkPorts(context.Background(), h)
	if err != nil || len(got) != 0 {
		t.Fatalf("systemd-resolved stub listener produced a finding: err=%v findings=%#v", err, got)
	}
}

// The file fallback is the normal path for the non-root Agent (sshd -T needs
// the root-only host keys) and the only path in observer mode. It must report
// the same global effective values as sshd -T: OpenSSH compiled defaults for
// unset keywords, and no Match-block value promoted to a global setting.
func TestSSHFallbackReportsEffectiveGlobalConfiguration(t *testing.T) {
	cases := []struct {
		name      string
		config    string
		wantTitle []string
		evidence  map[string]string
	}{
		{
			name:      "unset password authentication defaults to yes",
			config:    "#PasswordAuthentication yes\nPermitRootLogin no\nUsePAM yes\n",
			wantTitle: []string{"SSH password authentication is enabled"},
		},
		{
			name:      "unset root login defaults to prohibit-password",
			config:    "PasswordAuthentication no\n",
			wantTitle: []string{"Root SSH login is allowed with keys"},
			evidence:  map[string]string{"Root SSH login is allowed with keys": "PermitRootLogin prohibit-password"},
		},
		{
			name:      "match block value is not the global value",
			config:    "PermitRootLogin no\nSubsystem sftp internal-sftp\nMatch User sftponly\n    PasswordAuthentication no\n    ForceCommand internal-sftp\n",
			wantTitle: []string{"SSH password authentication is enabled"},
		},
		{
			name:      "conditional root login does not become a global yes",
			config:    "PasswordAuthentication no\nMatch\tAddress 192.0.2.0/24\n    PermitRootLogin yes\n",
			wantTitle: []string{"Root SSH login is allowed with keys"},
		},
		{
			name:      "explicit hardened globals stay clean",
			config:    "PasswordAuthentication no\nPermitRootLogin no\nMatch Address 192.0.2.0/24\n    PasswordAuthentication yes\n",
			wantTitle: nil,
		},
		{
			name:      "keyword=value form is a setting, not an unset keyword",
			config:    "PasswordAuthentication=no\nPermitRootLogin = no\n",
			wantTitle: nil,
		},
		{
			name:      "quoted arguments are unquoted",
			config:    "PasswordAuthentication \"no\"\nPermitRootLogin \"yes\"\n",
			wantTitle: []string{"Direct root SSH login is enabled"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeHost{files: map[string][]byte{"/etc/ssh/sshd_config": []byte(tc.config)}}
			got, err := checkSSH(context.Background(), h)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.wantTitle) {
				t.Fatalf("findings=%#v want titles %v", got, tc.wantTitle)
			}
			for i, title := range tc.wantTitle {
				if got[i].Title != title {
					t.Fatalf("finding %d title=%q want %q", i, got[i].Title, title)
				}
				if want, ok := tc.evidence[title]; ok && got[i].Evidence != want {
					t.Fatalf("finding %d evidence=%q want %q", i, got[i].Evidence, want)
				}
			}
		})
	}
}

// sshd -T already prints every effective keyword, so its output is used as is.
func TestSSHEffectiveProbeOutputIsAuthoritative(t *testing.T) {
	h := fakeHost{probes: map[Probe][]byte{ProbeSSHEffective: []byte("port 22\npasswordauthentication no\npermitrootlogin no\n")}}
	got, err := checkSSH(context.Background(), h)
	if err != nil || len(got) != 0 {
		t.Fatalf("err=%v findings=%#v", err, got)
	}
	h = fakeHost{probes: map[Probe][]byte{ProbeSSHEffective: []byte("passwordauthentication yes\npermitrootlogin without-password\n")}}
	got, err = checkSSH(context.Background(), h)
	if err != nil || len(got) != 2 || got[0].Severity != domain.SeverityHigh || got[1].Severity != domain.SeverityLow {
		t.Fatalf("err=%v findings=%#v", err, got)
	}
}

// An Include the parser cannot follow must keep the check incomplete, whatever
// separator it uses; otherwise the defaults above could be reported in place of
// values set by the included files.
func TestSSHIncludeWithTabOrEqualsIsUncertain(t *testing.T) {
	for _, config := range []string{"Include\t/etc/ssh/sshd_config.d/*.conf\n", "include=/etc/ssh/sshd_config.d/*.conf\n"} {
		h := fakeHost{files: map[string][]byte{"/etc/ssh/sshd_config": []byte(config)}}
		if got, err := checkSSH(context.Background(), h); err == nil {
			t.Fatalf("config %q: expected uncertainty error, got findings %#v", config, got)
		}
	}
}

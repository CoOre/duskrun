package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/CoOre/duskrun/internal/core"
	"github.com/CoOre/duskrun/internal/plugin"
)

// securityEvent reports an account event to the channels subscribed to
// security events. Delivery runs in the background: a login must not wait on
// an SMTP relay, and the alert does not depend on the request that caused it.
func (s *server) securityEvent(ctx context.Context, subject, msg string) {
	s.d.Log.Info("security event", "subject", subject, "message", msg)
	if s.d.Notify == nil {
		return
	}
	dctx := context.WithoutCancel(ctx)
	go s.d.Notify.SendSystem(dctx, plugin.EventSecurity, subject, msg)
}

// reportLockout reports the limiter keys a failed attempt just locked out.
func (s *server) reportLockout(ctx context.Context, locked []string, email, ip, why string) {
	for _, k := range locked {
		kind, value, _ := strings.Cut(k, ":")
		window := core.HumanDuration(s.limiter.window)
		switch kind {
		case "email", "mfa":
			from := ""
			if ip != "" {
				from = ", последняя с " + ip
			}
			s.securityEvent(ctx, email, fmt.Sprintf("%d неудачных попыток входа (%s)%s — вход в учётную запись заблокирован на %s",
				s.limiter.maxFail, why, from, window))
		case "ip", "mfa-ip":
			s.securityEvent(ctx, value, fmt.Sprintf("%d неудачных попыток входа с этого адреса (%s, последняя — %s) — адрес заблокирован на %s",
				s.limiter.maxFail, why, email, window))
		}
	}
}

// noteDevice remembers where the user signed in from and reports a browser or
// network not seen for this account before. A user's very first login is not
// reported: every device is new then, which says nothing.
func (s *server) noteDevice(ctx context.Context, user *core.User, ua, ip string) {
	fp, label := deviceOf(ua, ip)
	isNew, hadAny, err := s.d.Store.TouchKnownDevice(ctx, user.ID, fp, label, s.now())
	if err != nil {
		s.d.Log.Warn("record login device", "user", user.ID, "err", err)
		return
	}
	if isNew && hadAny {
		s.securityEvent(ctx, user.Email, "вход с нового устройства или из новой сети: "+label)
	}
}

// deviceOf fingerprints a login by browser family, OS and network. The full
// User-Agent would change with every browser update and the full address with
// every DHCP lease; both would make each login "new".
func deviceOf(ua, ip string) (fingerprint, label string) {
	browser, os := uaFamily(ua)
	network := ipPrefix(ip)
	sum := sha256.Sum256([]byte(browser + "\x00" + os + "\x00" + network))
	label = browser + " · " + os
	if ip != "" {
		label += ", " + ip
	}
	return hex.EncodeToString(sum[:]), label
}

// uaFamily names the browser and OS of a User-Agent, as the settings page
// does for push devices.
func uaFamily(ua string) (browser, os string) {
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "YaBrowser/"):
		browser = "Яндекс Браузер"
	case strings.Contains(ua, "Firefox/"), strings.Contains(ua, "FxiOS/"):
		browser = "Firefox"
	case strings.Contains(ua, "Chrome/"), strings.Contains(ua, "CriOS/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	case ua == "":
		browser = "без User-Agent"
	default:
		browser = "другой клиент"
	}
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		os = "iOS"
	case strings.Contains(ua, "Android"):
		os = "Android"
	case strings.Contains(ua, "Windows"):
		os = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		os = "macOS"
	case strings.Contains(ua, "CrOS"):
		os = "ChromeOS"
	case strings.Contains(ua, "Linux"):
		os = "Linux"
	default:
		os = "?"
	}
	return browser, os
}

// ipPrefix is the network a login came from: /24 for IPv4, /48 for IPv6.
func ipPrefix(ip string) string {
	addr := net.ParseIP(ip)
	if addr == nil {
		return ip
	}
	if v4 := addr.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return addr.Mask(net.CIDRMask(48, 128)).String() + "/48"
}

// userChange describes what an admin edit changed that matters for security:
// the role, the account being blocked or unblocked, a password reset.
func userChange(before, after *core.User, passwordReset bool) string {
	var parts []string
	if before.Role != after.Role {
		parts = append(parts, fmt.Sprintf("роль изменена: %s → %s", before.Role, after.Role))
	}
	if !before.Disabled && after.Disabled {
		parts = append(parts, "учётная запись заблокирована")
	}
	if before.Disabled && !after.Disabled {
		parts = append(parts, "учётная запись разблокирована")
	}
	if passwordReset {
		parts = append(parts, "пароль сброшен администратором")
	}
	return strings.Join(parts, "; ")
}

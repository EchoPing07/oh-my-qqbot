package message

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrMissingSessionIdentity mirrors the Python ValueError("UserID 或 GroupID
// 必须提供其一") raised when neither user_id nor group_id is supplied.
var ErrMissingSessionIdentity = errors.New("message: UserID 或 GroupID 必须提供其一")

// Route metadata keys, aligned with RouteKeyFactory.ACCOUNT_ID_KEYS /
// SCOPE_KEYS in 参考/MaiBot/src/platform_io/route_key_factory.py.
var (
	AccountIDKeys = []string{"platform_io_account_id", "account_id", "self_id", "bot_account"}
	ScopeKeys     = []string{"platform_io_scope", "route_scope", "adapter_scope", "connection_id"}
)

// SessionIDOptions carries the optional route components of calculate_session_id.
type SessionIDOptions struct {
	// UserID is required for private chats.
	UserID string
	// GroupID is required for group chats; when both are set the group wins.
	GroupID string
	// AccountID adds an `account:<id>` component when non-empty.
	AccountID string
	// Scope adds a `scope:<name>` component when non-empty.
	Scope string
}

// CalculateSessionID is a byte-exact port of
// SessionUtils.calculate_session_id in
// 参考/MaiBot/src/common/utils/utils_session.py:
//
//	components = [platform] + [f"account:{account_id}"] + [f"scope:{scope}"]
//	             + ([group_id] if group_id else [user_id, "private"])
//	session_id = md5("_".join(components)).hexdigest()
//
// Notes on fidelity:
//   - Truthiness is Python truthiness: only non-empty strings count, so an
//     empty account_id/scope contributes no component and an empty group_id
//     falls back to the private branch.
//   - The join separator is a single underscore, and there is no trailing
//     separator for the private branch because "private" is a component.
//   - The digest is the lowercase hex md5 of the UTF-8 bytes.
func CalculateSessionID(platform string, options SessionIDOptions) (string, error) {
	return CalculateSessionIDStrings(platform, options.UserID, options.GroupID, options.AccountID, options.Scope)
}

// CalculateSessionIDStrings is the raw form of CalculateSessionID, taking the
// four optional components positionally (mirroring the Python signature order
// used by callers: user_id, group_id, account_id, scope).
func CalculateSessionIDStrings(platform string, userID string, groupID string, accountID string, scope string) (string, error) {
	if strings.TrimSpace(userID) == "" && strings.TrimSpace(groupID) == "" {
		return "", ErrMissingSessionIdentity
	}

	routeComponents := make([]string, 0, 2)
	if accountID != "" {
		routeComponents = append(routeComponents, "account:"+accountID)
	}
	if scope != "" {
		routeComponents = append(routeComponents, "scope:"+scope)
	}

	var components []string
	if groupID != "" {
		components = append([]string{platform}, append(routeComponents, groupID)...)
	} else {
		components = append([]string{platform}, append(routeComponents, userID, "private")...)
	}
	return md5Hex(strings.Join(components, "_")), nil
}

// GroupSessionID is a convenience wrapper for group chats.
func GroupSessionID(platform string, groupID string, accountID string, scope string) (string, error) {
	return CalculateSessionIDStrings(platform, "", groupID, accountID, scope)
}

// PrivateSessionID is a convenience wrapper for private chats.
func PrivateSessionID(platform string, userID string, accountID string, scope string) (string, error) {
	return CalculateSessionIDStrings(platform, userID, "", accountID, scope)
}

// SessionRouteComponents mirrors RouteKeyFactory.extract_components: it picks
// the first non-empty account id / scope from a metadata map.
func SessionRouteComponents(metadata map[string]any) (accountID string, scope string) {
	return pickFirstString(metadata, AccountIDKeys), pickFirstString(metadata, ScopeKeys)
}

// SessionIDForMessage computes the session id of a message, mirroring
// ChatManager.register_message: the route components come from the message's
// additional_config, and group_id wins over user_id when both are present.
func SessionIDForMessage(message *SessionMessage) (string, error) {
	if message == nil {
		return "", ErrMissingSessionIdentity
	}
	accountID, scope := SessionRouteComponents(message.MessageInfo.AdditionalConfig)
	groupID := ""
	if message.MessageInfo.GroupInfo != nil {
		groupID = message.MessageInfo.GroupInfo.GroupID
	}
	return CalculateSessionIDStrings(
		message.Platform,
		message.MessageInfo.UserInfo.UserID,
		groupID,
		accountID,
		scope,
	)
}

// md5Hex returns the lowercase hex md5 of the UTF-8 encoded input.
func md5Hex(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

// SessionIDComponents exposes the exact component list (for tests/diagnostics).
func SessionIDComponents(platform string, userID string, groupID string, accountID string, scope string) []string {
	routeComponents := make([]string, 0, 2)
	if accountID != "" {
		routeComponents = append(routeComponents, "account:"+accountID)
	}
	if scope != "" {
		routeComponents = append(routeComponents, "scope:"+scope)
	}
	if groupID != "" {
		return append([]string{platform}, append(routeComponents, groupID)...)
	}
	return append([]string{platform}, append(routeComponents, userID, "private")...)
}

func pickFirstString(mapping map[string]any, keys []string) string {
	for _, key := range keys {
		raw, ok := mapping[key]
		if !ok || raw == nil {
			continue
		}
		normalized := strings.TrimSpace(toStringValue(raw))
		if normalized != "" {
			return normalized
		}
	}
	return ""
}

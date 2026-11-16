package mailoauth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

const GoogleScope = "https://mail.google.com/"
const MicrosoftScope = "https://outlook.office.com/IMAP.AccessAsUser.All"

type Error struct{ Code string }

func (err *Error) Error() string              { return Message(err.Code) }
func (err *Error) PublicMessage() string      { return err.Error() }
func (err *Error) OperationErrorCode() string { return err.Code }

func Failure(code string) error { return &Error{Code: code} }
func Code(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return "oauth_network_failed"
}
func Message(code string) string {
	switch code {
	case "oauth_config_missing":
		return "请先配置该服务商的邮箱 OAuth 应用和固定 HTTPS 回调地址"
	case "oauth_config_changed":
		return "授权配置或网络策略已变化，请重新发起授权"
	case "oauth_state_invalid":
		return "授权请求已过期、已使用或不属于当前会话，请重新发起授权"
	case "oauth_denied":
		return "你已取消服务商授权；可重新点击授权"
	case "oauth_identity_mismatch":
		return "授权账号与此邮箱或原绑定主体不一致，请登录原账号后重试"
	case "oauth_identity_invalid":
		return "服务商身份验证未通过，请重新授权并检查实例时间"
	case "oauth_scope_missing":
		return "未授予 IMAP 邮件权限，请重新授权并同意邮件权限；组织账号请联系管理员"
	case "oauth_reauthorization_required":
		return "邮箱授权已失效，请重新授权；Google Testing 应用的邮件授权通常仅持续七天"
	case "oauth_pending":
		return "邮箱尚未授权，请点击官方登录授权"
	case "oauth_rate_limited":
		return "服务商暂时限流，请稍后重试"
	case "oauth_provider_unavailable":
		return "服务商暂时不可用，请稍后重试"
	case "oauth_imap_denied":
		return "OAuth 授权成功但 IMAP 登录被拒绝，请检查邮箱地址、IMAP 开关和租户策略，再重新授权"
	case "oauth_config_invalid":
		return "OAuth 应用配置无效：请检查 client ID、密钥、HTTPS origin 和 Microsoft 账号类型"
	case "oauth_account_limit":
		return "已连接邮箱达到 10 个，请移除不再需要的连接后重新授权；待授权配置可以单独移除"
	default:
		return "邮箱授权网络请求失败，请检查所选代理、TLS、网络和回调配置后重试"
	}
}

type AppInput struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	Authority     string `json:"authority"`
	Origin        string `json:"origin"`
	SecretExpires int64  `json:"secret_expires"`
}
type App struct {
	Provider      string `json:"provider"`
	ClientID      string `json:"client_id"`
	Authority     string `json:"authority"`
	Origin        string `json:"origin"`
	Version       int64  `json:"version"`
	Configured    bool   `json:"configured"`
	Callback      string `json:"callback"`
	SecretExpires int64  `json:"secret_expires"`
	Secret        []byte `json:"-"`
}
type Account struct {
	ID            string `json:"-"`
	Provider      string `json:"-"`
	Subject       string `json:"-"`
	Tenant        string `json:"-"`
	Login         string `json:"-"`
	Scope         string `json:"-"`
	Refresh       []byte `json:"-"`
	Generation    int64  `json:"generation"`
	ConfigVersion int64  `json:"config_version"`
	Status        string `json:"status"`
	ErrorCode     string `json:"error_code,omitempty"`
}
type Token struct {
	Access  string
	Refresh string
	Scope   string
	IDToken string
	Expires time.Time
}
type Identity struct{ Subject, Tenant, Email string }
type Request struct {
	Account         Account
	App             App
	Verifier        string
	Nonce           string
	NetworkRevision string
	SessionHash     string
}

type Credential struct {
	Mechanism string
	Username  string
	Token     string
}
type Source interface {
	Credential(context.Context, string, string) (Credential, error)
}

func IsProvider(provider string) bool { return provider == "google" || provider == "microsoft" }
func Scope(provider string) string {
	if provider == "google" {
		return GoogleScope
	}
	return MicrosoftScope
}
func scopes(provider string) string {
	if provider == "google" {
		return "openid email " + GoogleScope
	}
	return "openid email profile offline_access " + MicrosoftScope
}
func callback(provider string) string { return "/api/mail-oauth/" + provider + "/callback" }
func validOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Path == "" && !strings.ContainsAny(origin, "\r\n\\")
}
func endpoints(app App) (string, string, string) {
	if app.Provider == "google" {
		return "https://accounts.google.com/o/oauth2/v2/auth", "https://oauth2.googleapis.com/token", "https://www.googleapis.com/oauth2/v3/certs"
	}
	base := "https://login.microsoftonline.com/" + app.Authority
	return base + "/oauth2/v2.0/authorize", base + "/oauth2/v2.0/token", base + "/discovery/v2.0/keys"
}

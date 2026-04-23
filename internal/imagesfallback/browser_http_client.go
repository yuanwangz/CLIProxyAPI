package imagesfallback

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	tls "github.com/bogdanfinn/utls"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
)

type webHTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
	Cookies(u *url.URL) []*http.Cookie
}

type tlsClientAdapter struct {
	client tlsclient.HttpClient
}

func newEdgeLikeClientProfile() profiles.ClientProfile {
	base := profiles.Chrome_105
	return profiles.NewClientProfile(
		tls.HelloEdge_106,
		base.GetSettings(),
		base.GetSettingsOrder(),
		base.GetPseudoHeaderOrder(),
		base.GetConnectionFlow(),
		base.GetPriorities(),
		base.GetHeaderPriority(),
		base.GetStreamID(),
		base.GetAllowHTTP(),
		base.GetHttp3Settings(),
		base.GetHttp3SettingsOrder(),
		base.GetHttp3PriorityParam(),
		base.GetHttp3PseudoHeaderOrder(),
		base.GetHttp3SendGreaseFrames(),
	)
}

func newProxyAwareClient(_ context.Context, cfg *sdkconfig.SDKConfig, auth *coreauth.Auth) (webHTTPClient, error) {
	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	proxySetting, err := proxyutil.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}

	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutMilliseconds(0),
		tlsclient.WithClientProfile(newEdgeLikeClientProfile()),
		tlsclient.WithRandomTLSExtensionOrder(),
		tlsclient.WithCookieJar(tlsclient.NewCookieJar()),
	}

	switch proxySetting.Mode {
	case proxyutil.ModeInherit, proxyutil.ModeDirect:
	case proxyutil.ModeProxy:
		options = append(options, tlsclient.WithProxyUrl(proxySetting.URL.String()))
	default:
		return nil, fmt.Errorf("unsupported proxy mode for images fallback: %v", proxySetting.Mode)
	}

	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("create browser-like http client: %w", err)
	}

	return &tlsClientAdapter{client: client}, nil
}

func (c *tlsClientAdapter) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("browser-like http client is unavailable")
	}
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}

	fReq, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, err
	}
	fReq.Header = cloneStdHeaderToFHTTP(req.Header)
	fReq.Host = req.Host
	fReq.ContentLength = req.ContentLength
	fReq.TransferEncoding = append([]string(nil), req.TransferEncoding...)
	fReq.Close = req.Close

	fResp, err := c.client.Do(fReq)
	if err != nil {
		return nil, err
	}

	return convertFHTTPResponse(fResp, req), nil
}

func (c *tlsClientAdapter) Cookies(u *url.URL) []*http.Cookie {
	if c == nil || c.client == nil || u == nil {
		return nil
	}

	cookies := c.client.GetCookies(u)
	if len(cookies) == 0 {
		return nil
	}

	out := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		out = append(out, &http.Cookie{
			Name:       cookie.Name,
			Value:      cookie.Value,
			Quoted:     cookie.Quoted,
			Path:       cookie.Path,
			Domain:     cookie.Domain,
			Expires:    cookie.Expires,
			RawExpires: cookie.RawExpires,
			MaxAge:     cookie.MaxAge,
			Secure:     cookie.Secure,
			HttpOnly:   cookie.HttpOnly,
			SameSite:   http.SameSite(cookie.SameSite),
			Raw:        cookie.Raw,
			Unparsed:   append([]string(nil), cookie.Unparsed...),
		})
	}

	return out
}

func cloneStdHeaderToFHTTP(src http.Header) fhttp.Header {
	if len(src) == 0 {
		return fhttp.Header{}
	}

	dst := make(fhttp.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func cloneFHTTPHeaderToStd(src fhttp.Header) http.Header {
	if len(src) == 0 {
		return http.Header{}
	}

	dst := make(http.Header, len(src))
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	return dst
}

func convertFHTTPResponse(resp *fhttp.Response, req *http.Request) *http.Response {
	if resp == nil {
		return nil
	}

	return &http.Response{
		Status:           resp.Status,
		StatusCode:       resp.StatusCode,
		Proto:            resp.Proto,
		ProtoMajor:       resp.ProtoMajor,
		ProtoMinor:       resp.ProtoMinor,
		Header:           cloneFHTTPHeaderToStd(resp.Header),
		Body:             resp.Body,
		ContentLength:    resp.ContentLength,
		TransferEncoding: append([]string(nil), resp.TransferEncoding...),
		Close:            resp.Close,
		Uncompressed:     resp.Uncompressed,
		Trailer:          cloneFHTTPHeaderToStd(resp.Trailer),
		Request:          req,
	}
}

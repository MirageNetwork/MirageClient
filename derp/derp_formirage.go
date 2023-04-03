package derp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go4.org/mem"
	"tailscale.com/control/controlclient"
	"tailscale.com/net/dnscache"
	"tailscale.com/net/dnsfallback"
	"tailscale.com/net/tlsdial"
	"tailscale.com/net/tsdial"
	"tailscale.com/net/tshttpproxy"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/util/multierr"
	"tailscale.com/util/singleflight"
)

func (s *Server) createHttpc() *http.Client {
	s.dialer = &tsdial.Dialer{Logf: s.logf}
	serverURL, err := url.Parse(s.ctrlURL)
	if err != nil {
		s.logf("failed to parse control URL %q: %v", s.ctrlURL, err)
		return nil
	}
	dnsCache := &dnscache.Resolver{
		Forward:          dnscache.Get().Forward, // use default cache's forwarder
		UseLastGood:      true,
		LookupIPFallback: dnsfallback.Lookup,
		Logf:             s.logf,
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = tshttpproxy.ProxyFromEnvironment
	tshttpproxy.SetTransportGetProxyConnectHeader(tr)
	tr.TLSClientConfig = tlsdial.Config(serverURL.Hostname(), tr.TLSClientConfig)
	tr.DialContext = dnscache.Dialer(s.dialer.SystemDial, dnsCache)
	tr.DialTLSContext = dnscache.TLSDialer(s.dialer.SystemDial, dnsCache, tr.TLSClientConfig)
	tr.ForceAttemptHTTP2 = true
	// Disable implicit gzip compression; the various
	// handlers (register, map, set-dns, etc) do their own
	// zstd compression per naclbox.
	tr.DisableCompression = true
	return &http.Client{Transport: tr}
}

// cgao6: 用以获取控制器的公钥
func (s *Server) loadServerPubKeys() (*tailcfg.OverTLSPublicKeyResponse, error) {
	keyURL := fmt.Sprintf("%v/key?v=%d", s.ctrlURL, tailcfg.CurrentCapabilityVersion)
	req, err := http.NewRequestWithContext(s.ctx, "GET", keyURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create control key request: %v", err)
	}
	res, err := s.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch control key: %v", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("fetch control key response: %v", err)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("fetch control key: %d", res.StatusCode)
	}
	var out tailcfg.OverTLSPublicKeyResponse
	jsonErr := json.Unmarshal(b, &out)
	if jsonErr == nil {
		return &out, nil
	}

	// Some old control servers might not be updated to send the new format.
	// Accept the old pre-JSON format too.
	out = tailcfg.OverTLSPublicKeyResponse{}
	k, err := key.ParseMachinePublicUntyped(mem.B(b))
	if err != nil {
		return nil, multierr.New(jsonErr, err)
	}
	out.LegacyPublicKey = k
	return &out, nil
}

func (s *Server) getNoiseClient() (*controlclient.NoiseClient, error) {
	var sfGroup singleflight.Group[struct{}, *controlclient.NoiseClient]
	nc, err, _ := sfGroup.Do(struct{}{}, func() (*controlclient.NoiseClient, error) {
		s.logf("creating new noise client")
		nc, err := controlclient.NewNoiseClient(s.naviPriKey, s.ctrlNoiseKey, s.ctrlURL, s.dialer, nil)
		if err != nil {
			return nil, err
		}
		return nc, nil
	})
	if err != nil {
		return nil, err
	}
	return nc, nil
}

func decode(res *http.Response, v any) error {
	defer res.Body.Close()
	msg, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("%d: %v", res.StatusCode, string(msg))
	}
	return json.Unmarshal(msg, v)
}

type RegisterRequest struct {
	ID        string
	Timestamp *time.Time
}

type NaviNode struct {
	ID           string `gorm:"primary_key;unique;not null" json:"Name"` //映射到DERPNode的Name
	NaviKey      string `json:"NaviKey"`                                 //记录DERPNode的MachineKey公钥
	NaviRegionID int    `gorm:"not null" json:"RegionID"`                //映射到DERPNode的RegionID
	HostName     string `json:"HostName"`                                //这个不需要独有，但是否必须域名呢？
	//这个不用？ CertName string `json:",omitempty"`
	IPv4        string `json:"IPv4"`        // 不是ipv4地址则失效，为none则禁用ipv4
	IPv6        string `json:"IPv6"`        // 不是ipv6地址则失效，为none则禁用ipv6
	NoSTUN      bool   `json:"NoSTUN"`      //禁用STUN
	STUNPort    int    `json:"STUNPort"`    //0代表3478，-1代表禁用
	NoDERP      bool   `json:"NoDERP"`      //禁用DERP
	DERPPort    int    `json:"DERPPort"`    //0代表443
	DNSProvider string `json:"DNSProvider"` //DNS服务商
	DNSID       string `json:"DNSID"`       //DNS服务商的ID
	DNSKey      string `json:"DNSKey"`      //DNS服务商的Key
	Arch        string `json:"Arch"`        //所在环境架构，x86_64或aarch64
}
type RegisterResponse struct {
	NodeInfo  NaviNode
	Timestamp *time.Time
}

func (s *Server) registerNaviToCtrl() (NaviNode, error) {
	now := time.Now().Round(time.Second)
	request := RegisterRequest{
		ID:        s.derpID,
		Timestamp: &now,
	}
	url := fmt.Sprintf("%s/navi/register", s.ctrlURL)
	url = strings.Replace(url, "http:", "https:", 1)
	bodyData, err := json.Marshal(request)
	if err != nil {
		return NaviNode{}, fmt.Errorf("register request: %w", err)
	}
	req, err := http.NewRequestWithContext(s.ctx, "POST", url, bytes.NewReader(bodyData))
	if err != nil {
		return NaviNode{}, fmt.Errorf("register request: %w", err)
	}
	res, err := s.nc.Do(req)
	if err != nil {
		return NaviNode{}, fmt.Errorf("register request: %w", err)
	}
	if res.StatusCode != 200 {
		msg, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return NaviNode{}, fmt.Errorf("register request: http %d: %.200s",
			res.StatusCode, strings.TrimSpace(string(msg)))
	}
	resp := RegisterResponse{} // TODO: 使用我们自己的司南节点注册响应
	if err := decode(res, &resp); err != nil {
		s.logf("error decoding RegisterResponse with server key %s and machine key %s: %v", s.ctrlNoiseKey, s.naviPubKey, err)
		return NaviNode{}, fmt.Errorf("register request: %v", err)
	}
	s.logf("register response: %v", resp)

	//TODO: 完成注册流程
	return resp.NodeInfo, nil
}

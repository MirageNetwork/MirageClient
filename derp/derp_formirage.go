package derp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/robfig/cron/v3"
	"tailscale.com/control/controlclient"
	"tailscale.com/net/dnscache"
	"tailscale.com/net/dnsfallback"
	"tailscale.com/net/tlsdial"
	"tailscale.com/net/tsdial"
	"tailscale.com/net/tshttpproxy"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/util/singleflight"
)

func (s *Server) createHttpc(dialer *tsdial.Dialer) *http.Client {
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
	tr.DialContext = dnscache.Dialer(dialer.SystemDial, dnsCache)
	tr.DialTLSContext = dnscache.TLSDialer(dialer.SystemDial, dnsCache, tr.TLSClientConfig)
	tr.ForceAttemptHTTP2 = true
	// Disable implicit gzip compression; the various
	// handlers (register, map, set-dns, etc) do their own
	// zstd compression per naclbox.
	tr.DisableCompression = true
	return &http.Client{Transport: tr}
}

// cgao6: 用以获取控制器的公钥
func (s *Server) prepareNoiseClient() error {
	keyURL := fmt.Sprintf("%v/key?v=%d", s.ctrlURL, tailcfg.CurrentCapabilityVersion)
	req, err := http.NewRequestWithContext(s.ctx, "GET", keyURL, nil)
	if err != nil {
		return fmt.Errorf("create control key request: %v", err)
	}
	dialer := &tsdial.Dialer{Logf: s.logf}
	httpc := s.createHttpc(dialer)
	res, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("fetch control key: %v", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("fetch control key response: %v", err)
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("fetch control key: %d", res.StatusCode)
	}
	var keys tailcfg.OverTLSPublicKeyResponse
	jsonErr := json.Unmarshal(b, &keys)
	if jsonErr != nil {
		return fmt.Errorf("fetch control key response: %v", jsonErr)
	}
	if !keys.PublicKey.IsZero() {
		httpc.CloseIdleConnections()
	}

	var sfGroup singleflight.Group[struct{}, *controlclient.NoiseClient]
	s.nc, err, _ = sfGroup.Do(struct{}{}, func() (*controlclient.NoiseClient, error) {
		s.logf("creating new noise client")
		nc, err := controlclient.NewNoiseClient(s.naviPriKey, keys.PublicKey, s.ctrlURL, dialer, nil)
		if err != nil {
			return nil, err
		}
		return nc, nil
	})
	if err != nil {
		return err
	}
	return nil
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

// 在受管情况下进行初始化
func (s *Server) PrepareManaged(url, id string, naviKey key.MachinePrivate) error {
	s.ctx = context.Background()
	s.ctrlURL = url
	s.derpID = id
	s.naviPriKey = naviKey
	s.trustNodesCache = cache.New(0, 0)
	s.Cronjob = cron.New()
	return s.prepareNoiseClient()
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

func (s *Server) TryLogin() (NaviNode, error) {
	request := tailcfg.RegisterRequest{}
	request.Auth.Provider = "Mirage"
	request.Auth.LoginName = s.derpID
	url := fmt.Sprintf("%s/machine/register", s.ctrlURL)
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
	resp := RegisterResponse{}
	if err := decode(res, &resp); err != nil {
		s.logf("error decoding RegisterResponse")
		return NaviNode{}, fmt.Errorf("register request: %v", err)
	}
	s.logf("register response: %v", resp)

	return resp.NodeInfo, nil
}

func (s *Server) UpdateNaviInfo(
	naviInfo NaviNode,
	hostname, addr, setIPv4, setIPv6, dnsProvider, dnsID, dnsKey *string,
	stunPort *int,
	runDERP, runSTUN *bool,
) error {
	*hostname = naviInfo.HostName
	if !naviInfo.NoDERP {
		*addr = ":" + strconv.Itoa(naviInfo.DERPPort)
	} else {
		*runDERP = false
	}
	if !naviInfo.NoSTUN {
		*stunPort = naviInfo.STUNPort
	} else {
		*runSTUN = false
	}
	*setIPv4 = naviInfo.IPv4
	*setIPv6 = naviInfo.IPv6
	*dnsProvider = naviInfo.DNSProvider
	*dnsID = naviInfo.DNSID
	*dnsKey = naviInfo.DNSKey
	return nil
}

type PullNodesListResponse struct {
	TrustNodesList map[string]string `json:"TrustNodesList"`
	Timestamp      *time.Time        `json:"Timestamp"`
}

func (s *Server) PullNodesList() error {
	request := tailcfg.MapRequest{}
	request.Hostinfo = &tailcfg.Hostinfo{
		FrontendLogID: "MirageNavi",
		BackendLogID:  s.derpID,
	}
	url := fmt.Sprintf("%s/navi/map", s.ctrlURL)
	url = strings.Replace(url, "http:", "https:", 1)
	bodyData, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("map request: %w", err)
	}
	req, err := http.NewRequestWithContext(s.ctx, "POST", url, bytes.NewReader(bodyData))
	if err != nil {
		return fmt.Errorf("map request: %w", err)
	}
	res, err := s.nc.Do(req)
	if err != nil {
		return fmt.Errorf("map request: %w", err)
	}
	if res.StatusCode != 200 {
		msg, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return fmt.Errorf("map request: http %d: %.200s",
			res.StatusCode, strings.TrimSpace(string(msg)))
	}

	resp := PullNodesListResponse{}
	if err := decode(res, &resp); err != nil {
		s.logf("error decoding TrustNodeList: %v", err)
		return fmt.Errorf("map request: %v", err)
	}
	s.logf("map response: %v", resp)

	expire := time.Now().Add(10 * time.Minute)
	s.trustNodesCache.Flush()
	for nkey := range resp.TrustNodesList {
		s.trustNodesCache.Set(nkey, struct{}{}, expire.Sub(time.Now()))
	}

	return nil
}

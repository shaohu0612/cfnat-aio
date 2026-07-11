// Package fofa FOFA API 客户端
//
// 设计目标：
//   - 多 key 轮换（绕过单账户配额）
//   - 手动触发（WebUI），不作为后台自动化
//   - 搜索结果作为 cmin2 候选 IP 池，喂给扫描器验证
package fofa

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"cfnat-aio/internal/config"
)

// Client FOFA 客户端
type Client struct {
	store  *config.SQLiteStore
	cfgMgr *config.Manager
	idx    int64 // 当前轮换位置
}

// New 创建 FOFA 客户端
func New(store *config.SQLiteStore, cfgMgr *config.Manager) *Client {
	return &Client{store: store, cfgMgr: cfgMgr}
}

// SearchResponse FOFA 搜索响应
type SearchResponse struct {
	Error   bool     `json:"error"`
	Errmsg  string   `json:"errmsg"`
	Mode    string   `json:"mode"`
	Query   string   `json:"query"`
	Page    int      `json:"page"`
	Size    int      `json:"size"`
	Total   int      `json:"total"`
	Results [][]string `json:"results"` // [[ip, port, ...], ...]
}

// Search 执行一次搜索
//   query: 搜索语法（FOFA 原生）
//   fields: 需要的字段，默认 "ip,port,protocol,country,asn,org"
func (c *Client) Search(query string, fields string) (*SearchResponse, error) {
	if fields == "" {
		fields = "ip,port,protocol"
	}
	key, kID, err := c.pickKey()
	if err != nil {
		return nil, err
	}

	apiURL := "https://fofa.info/api/v1/search/all"
	qbase64 := base64.StdEncoding.EncodeToString([]byte(query))
	form := url.Values{
		"email":   {key.Email},
		"key":     {key.Key},
		"qbase64": {qbase64},
		"fields":  {fields},
		"size":    {"100"},
		"page":    {"1"},
		"full":    {"false"},
	}

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.PostForm(apiURL, form)
	if err != nil {
		_ = c.store.LogFOFA(kID, query, "error", 0)
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var sr SearchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		_ = c.store.LogFOFA(kID, query, "parse_error", 0)
		return nil, fmt.Errorf("FOFA响应解析失败: %v, body: %s", err, string(body))
	}

	if sr.Error {
		_ = c.store.LogFOFA(kID, query, "api_error", 0)
		return &sr, fmt.Errorf("FOFA错误: %s", sr.Errmsg)
	}

	// 记录使用
	_ = c.store.IncrementFOFAUsage(kID, 1)
	_ = c.store.LogFOFA(kID, query, "ok", len(sr.Results))
	return &sr, nil
}

// pickKey 选择一个可用 key（轮换）
func (c *Client) pickKey() (config.FOFAKey, int64, error) {
	keys, _ := c.store.ListFOFAKeys()
	var enabled []config.FOFAKey
	for _, k := range keys {
		if k.Enabled {
			enabled = append(enabled, k)
		}
	}
	if len(enabled) == 0 {
		return config.FOFAKey{}, 0, fmt.Errorf("无可用 FOFA key")
	}
	idx := atomic.AddInt64(&c.idx, 1) - 1
	k := enabled[int(idx)%len(enabled)]
	return k, k.ID, nil
}

// 预设 FOFA 搜索模板
var PresetQueries = map[string]string{
	"CMIN2_HKG":  `asn="63150" && server="cloudflare" && country="HK"`,
	"CMIN2_SIN":  `asn="63150" && server="cloudflare" && country="SG"`,
	"CMIN2_LAX":  `asn="63150" && server="cloudflare" && country="US" && city="Los Angeles"`,
	"CMIN2_NRT":  `asn="63150" && server="cloudflare" && country="JP" && city="Tokyo"`,
	"CMIN2_KIX":  `asn="63150" && server="cloudflare" && country="JP" && city="Osaka"`,
	"CMIN2_FRA":  `asn="63150" && server="cloudflare" && country="DE" && city="Frankfurt"`,
	"CF_HKG":     `server="cloudflare" && country="HK" && port="443"`,
	"CF_SIN":     `server="cloudflare" && country="SG" && port="443"`,
	"CF_LAX":     `server="cloudflare" && country="US" && city="Los Angeles" && port="443"`,
}

// IPEntry 提取 IP 条目
type IPEntry struct {
	IP       string
	Port     string
	Protocol string
	Region   string // 推断的地区（基于查询名）
}

// ExtractIPs 从搜索结果提取 IP 列表
func (c *Client) ExtractIPs(resp *SearchResponse, regionHint string) []IPEntry {
	var out []IPEntry
	for _, row := range resp.Results {
		if len(row) < 1 {
			continue
		}
		ip := row[0]
		port := "443"
		if len(row) > 1 {
			port = row[1]
		}
		protocol := "https"
		if len(row) > 2 {
			protocol = row[2]
		}
		out = append(out, IPEntry{
			IP:       ip,
			Port:     port,
			Protocol: protocol,
			Region:   regionHint,
		})
	}
	return out
}

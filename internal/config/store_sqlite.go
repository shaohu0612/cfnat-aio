// Package config 提供基于 SQLite 的配置持久化实现
package config

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

// SQLiteStore SQLite 配置存储
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore 创建并初始化 SQLite 配置存储
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	s := &SQLiteStore{db: db}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) init() error {
	schema := `
	CREATE TABLE IF NOT EXISTS kv_config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS iplib_ip (
		ip TEXT NOT NULL,
		region TEXT NOT NULL,
		colo TEXT,
		speed_mbps REAL,
		latency_ms REAL,
		source TEXT,
		added_at TEXT,
		last_check TEXT,
		last_ok INTEGER,
		fail_count INTEGER DEFAULT 0,
		note TEXT,
		PRIMARY KEY (ip, region)
	);
	CREATE TABLE IF NOT EXISTS iplib_meta (
		ip TEXT NOT NULL,
		region TEXT NOT NULL,
		cidr24 TEXT,
		tested_count INTEGER DEFAULT 0,
		last_test TEXT,
		last_ok INTEGER,
		avg_speed_mbps REAL,
		PRIMARY KEY (ip, region)
	);
	CREATE TABLE IF NOT EXISTS scan_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		started_at TEXT,
		finished_at TEXT,
		status TEXT,
		total INTEGER,
		passed INTEGER,
		stats_json TEXT
	);
	CREATE TABLE IF NOT EXISTS cidr_colo_map (
		cidr24     TEXT PRIMARY KEY,
		colo       TEXT NOT NULL,
		probed_at  TEXT,
		ok_count   INTEGER DEFAULT 0,
		fail_count INTEGER DEFAULT 0,
		confidence REAL DEFAULT 0.0
	);
	CREATE INDEX IF NOT EXISTS idx_cidr_colo ON cidr_colo_map(colo);
	CREATE TABLE IF NOT EXISTS colo_scan_state (
		colo        TEXT PRIMARY KEY,
		region      TEXT NOT NULL,
		budget      INTEGER DEFAULT 10,
		current_ips INTEGER DEFAULT 0,
		target_ips  INTEGER DEFAULT 5,
		last_scan   TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_colo_region ON colo_scan_state(region);
	CREATE TABLE IF NOT EXISTS cf_datacenter (
		colo         TEXT PRIMARY KEY,
		name         TEXT,
		country      TEXT,
		city         TEXT,
		continent    TEXT,
		latitude     REAL,
		longitude    REAL,
		region_name  TEXT,
		zone         TEXT,
		updated_at   TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_cf_country ON cf_datacenter(country);
	CREATE INDEX IF NOT EXISTS idx_cf_region ON cf_datacenter(region_name);
	CREATE TABLE IF NOT EXISTS cf_datacenter_meta (
		key         TEXT PRIMARY KEY,
		value       TEXT NOT NULL,
		updated_at  TEXT
	);
	CREATE TABLE IF NOT EXISTS ip_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ip TEXT NOT NULL,
		region TEXT NOT NULL,
		event_type TEXT NOT NULL,
		details TEXT,
		created_at TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_ip_history_ip ON ip_history(ip);
	CREATE INDEX IF NOT EXISTS idx_ip_history_region ON ip_history(region);
	`
	_, err := s.db.Exec(schema)
	return err
}

// === KV 读写辅助 ===

func (s *SQLiteStore) setKV(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO kv_config(key,value) VALUES(?,?)`, key, value)
	return err
}

func (s *SQLiteStore) getKV(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM kv_config WHERE key=?`, key).Scan(&v)
	return v, err
}

// === ConfigStore 接口实现 ===

func (s *SQLiteStore) LoadGeneral() (GeneralConfig, error) {
	v, err := s.getKV("general")
	if err != nil {
		return GeneralConfig{}, err
	}
	var g GeneralConfig
	err = json.Unmarshal([]byte(v), &g)
	return g, err
}

func (s *SQLiteStore) SaveGeneral(g GeneralConfig) error {
	b, _ := json.Marshal(g)
	return s.setKV("general", string(b))
}

func (s *SQLiteStore) LoadScanner() (ScannerConfig, error) {
	v, err := s.getKV("scanner")
	if err != nil {
		return ScannerConfig{}, err
	}
	var sc ScannerConfig
	err = json.Unmarshal([]byte(v), &sc)
	return sc, err
}

func (s *SQLiteStore) SaveScanner(sc ScannerConfig) error {
	b, _ := json.Marshal(sc)
	return s.setKV("scanner", string(b))
}

func (s *SQLiteStore) LoadCfnat() (CfnatConfig, error) {
	v, err := s.getKV("cfnat")
	if err != nil {
		return CfnatConfig{}, err
	}
	var c CfnatConfig
	err = json.Unmarshal([]byte(v), &c)
	return c, err
}

func (s *SQLiteStore) SaveCfnat(c CfnatConfig) error {
	b, _ := json.Marshal(c)
	return s.setKV("cfnat", string(b))
}

func (s *SQLiteStore) LoadProxyForward() (ProxyForwardConfig, error) {
	v, err := s.getKV("proxy_forward")
	if err != nil {
		return ProxyForwardConfig{}, err
	}
	var pf ProxyForwardConfig
	err = json.Unmarshal([]byte(v), &pf)
	return pf, err
}

func (s *SQLiteStore) SaveProxyForward(pf ProxyForwardConfig) error {
	b, _ := json.Marshal(pf)
	return s.setKV("proxy_forward", string(b))
}

func (s *SQLiteStore) LoadRegions() ([]ProxyRegion, error) {
	v, err := s.getKV("regions")
	if err != nil {
		return nil, err
	}
	var rs []ProxyRegion
	err = json.Unmarshal([]byte(v), &rs)
	return rs, err
}

func (s *SQLiteStore) SaveRegions(regions []ProxyRegion) error {
	b, _ := json.Marshal(regions)
	return s.setKV("regions", string(b))
}

// === IP 库读写 ===

type IPEntry struct {
	IP        string  `json:"ip"`
	Region    string  `json:"region"`
	Colo      string  `json:"colo"`
	SpeedMbps float64 `json:"speed_mbps"`
	LatencyMs float64 `json:"latency_ms"`
	Source    string  `json:"source"`
	AddedAt   string  `json:"added_at"`
	LastCheck string  `json:"last_check"`
	LastOK    bool    `json:"last_ok"`
	FailCount int     `json:"fail_count"`
	Note      string  `json:"note"`
}

type IPMeta struct {
	IP           string
	Region       string
	CIDR24       string
	TestedCount  int
	LastTest     string
	LastOK       bool
	AvgSpeedMbps float64
}

func (s *SQLiteStore) UpsertIP(e IPEntry) error {
	if e.AddedAt == "" {
		e.AddedAt = NowISO()
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO iplib_ip
		(ip,region,colo,speed_mbps,latency_ms,source,added_at,last_check,last_ok,fail_count,note)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		e.IP, e.Region, e.Colo, e.SpeedMbps, e.LatencyMs, e.Source,
		e.AddedAt, e.LastCheck, e.LastOK, e.FailCount, e.Note)
	return err
}

func (s *SQLiteStore) UpsertIPsBatch(entries []IPEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO iplib_ip
		(ip,region,colo,speed_mbps,latency_ms,source,added_at,last_check,last_ok,fail_count,note)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if e.AddedAt == "" {
			e.AddedAt = NowISO()
		}
		_, err := stmt.Exec(e.IP, e.Region, e.Colo, e.SpeedMbps, e.LatencyMs, e.Source,
			e.AddedAt, e.LastCheck, e.LastOK, e.FailCount, e.Note)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) DeleteIP(ip, region string) error {
	_, err := s.db.Exec(`DELETE FROM iplib_ip WHERE ip=? AND region=?`, ip, region)
	return err
}

func (s *SQLiteStore) ListIPs(region string) ([]IPEntry, error) {
	rows, err := s.db.Query(`SELECT ip,region,colo,COALESCE(speed_mbps,0),COALESCE(latency_ms,0),
		COALESCE(source,''),COALESCE(added_at,''),COALESCE(last_check,''),
		COALESCE(last_ok,0),COALESCE(fail_count,0),COALESCE(note,'')
		FROM iplib_ip WHERE region=? ORDER BY speed_mbps DESC`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPEntry
	for rows.Next() {
		var e IPEntry
		if err := rows.Scan(&e.IP, &e.Region, &e.Colo, &e.SpeedMbps, &e.LatencyMs,
			&e.Source, &e.AddedAt, &e.LastCheck, &e.LastOK, &e.FailCount, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *SQLiteStore) ListAllIPs() ([]IPEntry, error) {
	rows, err := s.db.Query(`SELECT ip,region,colo,COALESCE(speed_mbps,0),COALESCE(latency_ms,0),
		COALESCE(source,''),COALESCE(added_at,''),COALESCE(last_check,''),
		COALESCE(last_ok,0),COALESCE(fail_count,0),COALESCE(note,'')
		FROM iplib_ip ORDER BY region, speed_mbps DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPEntry
	for rows.Next() {
		var e IPEntry
		if err := rows.Scan(&e.IP, &e.Region, &e.Colo, &e.SpeedMbps, &e.LatencyMs,
			&e.Source, &e.AddedAt, &e.LastCheck, &e.LastOK, &e.FailCount, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *SQLiteStore) MarkIPChecked(ip, region string, ok bool, speed, latency float64) error {
	_, err := s.db.Exec(`UPDATE iplib_ip SET last_check=?, last_ok=?,
		speed_mbps=?, latency_ms=?,
		fail_count = CASE WHEN ?=1 THEN 0 ELSE fail_count+1 END
		WHERE ip=? AND region=?`,
		NowISO(), ok, speed, latency, ok, ip, region)
	return err
}

func (s *SQLiteStore) RemoveFailingIPs(maxFails int) (int, error) {
	res, err := s.db.Exec(`DELETE FROM iplib_ip WHERE fail_count >= ?`, maxFails)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLiteStore) UpsertMeta(m IPMeta) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO iplib_meta
		(ip,region,cidr24,tested_count,last_test,last_ok,avg_speed_mbps)
		VALUES(?,?,?,?,?,?,?)`,
		m.IP, m.Region, m.CIDR24, m.TestedCount, m.LastTest, m.LastOK, m.AvgSpeedMbps)
	return err
}

func (s *SQLiteStore) GetMeta(ip, region string) (*IPMeta, error) {
	row := s.db.QueryRow(`SELECT ip,region,COALESCE(cidr24,''),COALESCE(tested_count,0),
		COALESCE(last_test,''),COALESCE(last_ok,0),COALESCE(avg_speed_mbps,0)
		FROM iplib_meta WHERE ip=? AND region=?`, ip, region)
	var m IPMeta
	err := row.Scan(&m.IP, &m.Region, &m.CIDR24, &m.TestedCount, &m.LastTest, &m.LastOK, &m.AvgSpeedMbps)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

type ScanHistory struct {
	ID         int64  `json:"id"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Status     string `json:"status"`
	Total      int    `json:"total"`
	Passed     int    `json:"passed"`
	StatsJSON  string `json:"stats_json"`
}

func (s *SQLiteStore) AddScanHistory(h ScanHistory) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO scan_history(started_at,finished_at,status,total,passed,stats_json)
		VALUES(?,?,?,?,?,?)`, h.StartedAt, h.FinishedAt, h.Status, h.Total, h.Passed, h.StatsJSON)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *SQLiteStore) ListScanHistory(limit int) ([]ScanHistory, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,COALESCE(started_at,''),COALESCE(finished_at,''),
		COALESCE(status,''),COALESCE(total,0),COALESCE(passed,0),COALESCE(stats_json,'')
		FROM scan_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScanHistory
	for rows.Next() {
		var h ScanHistory
		if err := rows.Scan(&h.ID, &h.StartedAt, &h.FinishedAt,
			&h.Status, &h.Total, &h.Passed, &h.StatsJSON); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// DB 暴露内部 db 句柄（仅供 scanner 写 scan_history 状态时使用）
func (s *SQLiteStore) DB() *sql.DB { return s.db }

func (s *SQLiteStore) String() string {
	return fmt.Sprintf("SQLiteStore@%p", s)
}

// === 数据中心字典 ===

type DatacenterEntry struct {
	Colo       string  `json:"colo"`
	Name       string  `json:"name"`
	Country    string  `json:"country"`
	City       string  `json:"city"`
	Continent  string  `json:"continent"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	RegionName string  `json:"region_name"`
	Zone       string  `json:"zone"`
	UpdatedAt  string  `json:"updated_at"`
}

func (s *SQLiteStore) UpsertDatacenter(e DatacenterEntry) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO cf_datacenter
		(colo,name,country,city,continent,latitude,longitude,region_name,zone,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		e.Colo, e.Name, e.Country, e.City, e.Continent, e.Latitude, e.Longitude,
		e.RegionName, e.Zone, e.UpdatedAt)
	return err
}

func (s *SQLiteStore) UpsertDatacentersBatch(entries []DatacenterEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO cf_datacenter
		(colo,name,country,city,continent,latitude,longitude,region_name,zone,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range entries {
		if e.UpdatedAt == "" {
			e.UpdatedAt = NowISO()
		}
		_, err := stmt.Exec(e.Colo, e.Name, e.Country, e.City, e.Continent, e.Latitude, e.Longitude,
			e.RegionName, e.Zone, e.UpdatedAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListDatacenters() ([]DatacenterEntry, error) {
	rows, err := s.db.Query(`SELECT colo,COALESCE(name,''),COALESCE(country,''),COALESCE(city,''),
		COALESCE(continent,''),COALESCE(latitude,0),COALESCE(longitude,0),
		COALESCE(region_name,''),COALESCE(zone,''),COALESCE(updated_at,'')
		FROM cf_datacenter ORDER BY country, city`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DatacenterEntry
	for rows.Next() {
		var e DatacenterEntry
		if err := rows.Scan(&e.Colo, &e.Name, &e.Country, &e.City, &e.Continent,
			&e.Latitude, &e.Longitude, &e.RegionName, &e.Zone, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *SQLiteStore) ListDatacentersByCountry(country string) ([]DatacenterEntry, error) {
	rows, err := s.db.Query(`SELECT colo,COALESCE(name,''),COALESCE(country,''),COALESCE(city,''),
		COALESCE(continent,''),COALESCE(latitude,0),COALESCE(longitude,0),
		COALESCE(region_name,''),COALESCE(zone,''),COALESCE(updated_at,'')
		FROM cf_datacenter WHERE country=? OR region_name=? ORDER BY city`, country, country)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DatacenterEntry
	for rows.Next() {
		var e DatacenterEntry
		if err := rows.Scan(&e.Colo, &e.Name, &e.Country, &e.City, &e.Continent,
			&e.Latitude, &e.Longitude, &e.RegionName, &e.Zone, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *SQLiteStore) GetDatacenter(colo string) (*DatacenterEntry, error) {
	row := s.db.QueryRow(`SELECT colo,COALESCE(name,''),COALESCE(country,''),COALESCE(city,''),
		COALESCE(continent,''),COALESCE(latitude,0),COALESCE(longitude,0),
		COALESCE(region_name,''),COALESCE(zone,''),COALESCE(updated_at,'')
		FROM cf_datacenter WHERE colo=?`, colo)
	var e DatacenterEntry
	err := row.Scan(&e.Colo, &e.Name, &e.Country, &e.City, &e.Continent,
		&e.Latitude, &e.Longitude, &e.RegionName, &e.Zone, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *SQLiteStore) ListCountries() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT country FROM cf_datacenter WHERE country!='' ORDER BY country`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *SQLiteStore) SetDatacenterMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO cf_datacenter_meta(key,value,updated_at) VALUES(?,?,?)`,
		key, value, NowISO())
	return err
}

func (s *SQLiteStore) GetDatacenterMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM cf_datacenter_meta WHERE key=?`, key).Scan(&v)
	return v, err
}

// === IP 历史记录 ===

type IPHistoryEntry struct {
	ID        int64  `json:"id"`
	IP        string `json:"ip"`
	Region    string `json:"region"`
	EventType string `json:"event_type"`
	Details   string `json:"details"`
	CreatedAt string `json:"created_at"`
}

func (s *SQLiteStore) AddIPHistory(ip, region, eventType, details string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO ip_history(ip,region,event_type,details,created_at) VALUES(?,?,?,?,?)`,
		ip, region, eventType, details, NowISO())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *SQLiteStore) ListIPHistory(ip, region string, limit int) ([]IPHistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,ip,region,event_type,COALESCE(details,''),COALESCE(created_at,'')
		FROM ip_history WHERE ip=? AND region=? ORDER BY id DESC LIMIT ?`, ip, region, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPHistoryEntry
	for rows.Next() {
		var e IPHistoryEntry
		if err := rows.Scan(&e.ID, &e.IP, &e.Region, &e.EventType, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *SQLiteStore) ListIPHistoryByRegion(region string, limit int) ([]IPHistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,ip,region,event_type,COALESCE(details,''),COALESCE(created_at,'')
		FROM ip_history WHERE region=? ORDER BY id DESC LIMIT ?`, region, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPHistoryEntry
	for rows.Next() {
		var e IPHistoryEntry
		if err := rows.Scan(&e.ID, &e.IP, &e.Region, &e.EventType, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *SQLiteStore) PruneIPHistory(days int) (int, error) {
	res, err := s.db.Exec(`DELETE FROM ip_history WHERE created_at < DATETIME('now', ?)`,
		fmt.Sprintf("-%d days", days))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

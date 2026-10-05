package model_proxy

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func (r *Repository) ListEgressProxies(ctx context.Context, userID, accountID string) ([]EgressProxy, error) {
	rows, err := r.db.Query(ctx, `
		SELECT p.id,p.user_id,p.admin_account_id,p.name,p.protocol,p.address,p.url_ciphertext,p.enabled,
			(SELECT count(*) FROM proxy_routes r WHERE r.egress_proxy_id=p.id),
			p.last_test_status,p.last_test_latency_ms,p.last_test_exit_ip,p.last_test_error,p.last_tested_at,p.created_at,p.updated_at
		FROM model_egress_proxies p
		WHERE p.user_id=$1 AND p.admin_account_id=$2
		ORDER BY p.created_at ASC,p.id ASC
	`, userID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]EgressProxy, 0)
	for rows.Next() {
		proxy, scanErr := scanEgressProxy(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, proxy)
	}
	return result, rows.Err()
}

func (r *Repository) GetEgressProxy(ctx context.Context, proxyID string) (*EgressProxy, error) {
	row := r.db.QueryRow(ctx, `
		SELECT p.id,p.user_id,p.admin_account_id,p.name,p.protocol,p.address,p.url_ciphertext,p.enabled,
			(SELECT count(*) FROM proxy_routes r WHERE r.egress_proxy_id=p.id),
			p.last_test_status,p.last_test_latency_ms,p.last_test_exit_ip,p.last_test_error,p.last_tested_at,p.created_at,p.updated_at
		FROM model_egress_proxies p WHERE p.id=$1
	`, proxyID)
	proxy, err := scanEgressProxy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &proxy, err
}

func scanEgressProxy(row routeScanner) (EgressProxy, error) {
	var proxy EgressProxy
	err := row.Scan(&proxy.ID, &proxy.UserID, &proxy.AdminAccountID, &proxy.Name, &proxy.Protocol, &proxy.Address,
		&proxy.URLCiphertext, &proxy.Enabled, &proxy.RouteCount, &proxy.LastTestStatus, &proxy.LastTestLatencyMS,
		&proxy.LastTestExitIP, &proxy.LastTestError, &proxy.LastTestedAt, &proxy.CreatedAt, &proxy.UpdatedAt)
	return proxy, err
}

func (r *Repository) CreateEgressProxy(ctx context.Context, proxy EgressProxy) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO model_egress_proxies (id,user_id,admin_account_id,name,protocol,address,url_ciphertext,enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, proxy.ID, proxy.UserID, proxy.AdminAccountID, proxy.Name, proxy.Protocol, proxy.Address, proxy.URLCiphertext, proxy.Enabled)
	return err
}

func (r *Repository) UpdateEgressProxy(ctx context.Context, proxy EgressProxy) error {
	result, err := r.db.Exec(ctx, `
		UPDATE model_egress_proxies SET name=$4,protocol=$5,address=$6,url_ciphertext=$7,enabled=$8,
			last_test_status=$9,last_test_latency_ms=$10,last_test_exit_ip=$11,last_test_error=$12,last_tested_at=$13,updated_at=now()
		WHERE id=$1 AND user_id=$2 AND admin_account_id=$3
	`, proxy.ID, proxy.UserID, proxy.AdminAccountID, proxy.Name, proxy.Protocol, proxy.Address, proxy.URLCiphertext,
		proxy.Enabled, proxy.LastTestStatus, proxy.LastTestLatencyMS, proxy.LastTestExitIP, proxy.LastTestError, proxy.LastTestedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *Repository) DeleteEgressProxy(ctx context.Context, userID, accountID, proxyID string) error {
	var routeCount int
	if err := r.db.QueryRow(ctx, `SELECT count(*) FROM proxy_routes WHERE egress_proxy_id=$1 AND user_id=$2 AND admin_account_id=$3`, proxyID, userID, accountID).Scan(&routeCount); err != nil {
		return err
	}
	if routeCount > 0 {
		return &requestError{Status: 409, Message: "model proxy is still used by one or more routes"}
	}
	result, err := r.db.Exec(ctx, `DELETE FROM model_egress_proxies WHERE id=$1 AND user_id=$2 AND admin_account_id=$3`, proxyID, userID, accountID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *Repository) SaveEgressProxyTest(ctx context.Context, proxyID string, result EgressProxyTestResult) error {
	status := "failed"
	var latency *int64
	exitIP, message := "", result.Message
	if result.Success {
		status, latency, exitIP, message = "healthy", &result.LatencyMS, result.ExitIP, ""
	}
	_, err := r.db.Exec(ctx, `
		UPDATE model_egress_proxies SET last_test_status=$2,last_test_latency_ms=$3,last_test_exit_ip=$4,
			last_test_error=$5,last_tested_at=now(),updated_at=now() WHERE id=$1
	`, proxyID, status, latency, exitIP, message)
	return err
}

package promclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

type PromClient struct {
	v1api v1.API
}

func NewPromClient(url string) (*PromClient, error) {
	client, err := api.NewClient(api.Config{Address: url})
	if err != nil {
		return nil, err
	}
	return &PromClient{v1api: v1.NewAPI(client)}, nil
}

func (p *PromClient) QueryAvgCPU(ctx context.Context, namespace string, podRegex string, window string) (float64, error) {
	query := fmt.Sprintf(`
      100 *
      avg(
        sum by(pod) (rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s",container!="POD",container!=""}[%s]))
        /
        sum by(pod) (kube_pod_container_resource_requests{namespace="%s",pod=~"%s",resource="cpu"})
      )`, namespace, podRegex, window, namespace, podRegex)

	result, _, err := p.v1api.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("prometheus query error: %w; query=%s", err, query)
	}

	vector, ok := result.(model.Vector)
	if !ok || vector.Len() == 0 {
		return 0, fmt.Errorf("no data returned for query: %s", query)
	}

	return float64(vector[0].Value), nil
}

func (p *PromClient) QueryAvgIOWait(ctx context.Context, endpoints []string, window string) (float64, error) {
	nodeRegex := strings.Join(endpoints, "|")
	query := fmt.Sprintf(`
	  100 * 
	  avg (
	  sum by (instance) (rate(node_cpu_seconds_total{mode="iowait",instance=~"%s"}[%s]))
	  /
	  sum by (instance) (rate(node_cpu_seconds_total{instance=~"%s"}[%s]))
	)`, nodeRegex, window, nodeRegex, window)

	result, _, err := p.v1api.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("prometheus query error: %w; query=%s", err, query)
	}

	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return 0, fmt.Errorf("no data returned for query: %s", query)
	}

	return float64(vector[0].Value), nil
}

func (p *PromClient) QueryAvgWriteRatio(ctx context.Context, window string) (float64, error) {
	query := fmt.Sprintf(`
	100 *
	rate(mongodb_collstats_latencyStats_writes_ops[%s])
	/ 
	(
	  rate(mongodb_collstats_latencyStats_writes_ops[%s])
	  + 
	  rate(mongodb_collstats_latencyStats_reads_ops[%s])
	)
	`, window, window, window)

	result, _, err := p.v1api.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("prometheus query error: %w; query=%s", err, query)
	}

	vector, ok := result.(model.Vector)
	if !ok || len(vector) == 0 {
		return 0, fmt.Errorf("no data returned for query: %s", query)
	}

	return float64(vector[0].Value), nil
}

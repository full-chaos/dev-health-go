//go:build integration

package readers_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	clickhousedriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// containerClickHouseImage is the server CI's readers-integration job runs
// (.github/workflows/ci.yml).
const containerClickHouseImage = "clickhouse/clickhouse-server:26.7"

// TestMain gives the integration tests a DSN from a throwaway ClickHouse
// container when DEV_HEALTH_GO_CLICKHOUSE_CONTAINER=1 and no DSN is set. CI
// sets the DSN itself and seeds its database, so it never starts a container;
// the container database holds no CI seed, so run it for the tests that build
// their own database.
func TestMain(m *testing.M) {
	os.Exit(runWithContainerClickHouse(m))
}

func runWithContainerClickHouse(m *testing.M) int {
	if os.Getenv("ACR_CLICKHOUSE_INTEGRATION_DSN") != "" || os.Getenv("DEV_HEALTH_GO_CLICKHOUSE_CONTAINER") != "1" {
		return m.Run()
	}
	ctx := context.Background()
	const user, password, database = "readers_it", "readers_it_pw", "dev_health_go_readers_container"
	container, err := testcontainers.Run(ctx, containerClickHouseImage,
		testcontainers.WithExposedPorts("8123/tcp", "9000/tcp"),
		testcontainers.WithEnv(map[string]string{"CLICKHOUSE_USER": user, "CLICKHOUSE_PASSWORD": password}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/ping").WithPort("8123/tcp")),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start %s: %v\n", containerClickHouseImage, err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(container) }()
	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "container host: %v\n", err)
		return 1
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "container port: %v\n", err)
		return 1
	}
	dsn := fmt.Sprintf("clickhouse://%s:%s@%s:%s/%s", user, password, host, port.Port(), database)
	options, err := clickhousedriver.ParseDSN(fmt.Sprintf("clickhouse://%s:%s@%s:%s/default", user, password, host, port.Port()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse container DSN: %v\n", err)
		return 1
	}
	admin, err := clickhousedriver.Open(options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open container admin connection: %v\n", err)
		return 1
	}
	err = admin.Exec(ctx, "CREATE DATABASE "+database)
	_ = admin.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", database, err)
		return 1
	}
	for name, value := range map[string]string{
		"ACR_CLICKHOUSE_INTEGRATION_DSN":      dsn,
		"ACR_CLICKHOUSE_INTEGRATION_DATABASE": database,
		"ACR_CLICKHOUSE_INTEGRATION_ISOLATED": "1",
		"ACR_CLICKHOUSE_INTEGRATION_REQUIRED": "1",
	} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", name, err)
			return 1
		}
	}
	return m.Run()
}

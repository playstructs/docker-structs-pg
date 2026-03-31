package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

type EntityConfig struct {
	Endpoint     string
	DataKey      string
	CompositeKey string
}

var entities = []EntityConfig{
	{"allocation", "Allocation", "structs.structs.EventAllocation.allocation"},
	{"agreement", "Agreement", "structs.structs.EventAgreement.agreement"},
	{"fleet", "Fleet", "structs.structs.EventFleet.fleet"},
	{"guild", "Guild", "structs.structs.EventGuild.guild"},
	{"infusion", "Infusion", "structs.structs.EventInfusion.infusion"},
	{"planet", "Planet", "structs.structs.EventPlanet.planet"},
	{"planet_attribute", "planetAttributeRecords", "structs.structs.EventPlanetAttribute.planetAttributeRecord"},
	{"player", "Player", "structs.structs.EventPlayer.player"},
	{"reactor", "Reactor", "structs.structs.EventReactor.reactor"},
	{"provider", "Provider", "structs.structs.EventProvider.provider"},
	{"struct_type", "StructType", "structs.structs.EventStructType.structType"},
	{"struct", "Struct", "structs.structs.EventStruct.structure"},
	{"struct_attribute", "structAttributeRecords", "structs.structs.EventStructAttribute.structAttributeRecord"},
	{"substation", "Substation", "structs.structs.EventSubstation.substation"},
	{"address", "address", "structs.structs.EventAddress.address"},
	{"grid", "gridRecords", "structs.structs.EventGrid.gridRecord"},
	{"permission", "permissionRecords", "structs.structs.EventPermission.permissionRecord"},
	{"guild_membership_application", "guildMembershipApplication", "structs.structs.EventGuildMembershipApplication.guildMembershipApplication"},
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func buildDatabaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	host := envOrDefault("PGHOST", "localhost")
	port := envOrDefault("PGPORT", "5432")
	user := envOrDefault("PGUSER", "structs")
	db := envOrDefault("PGDATABASE", "structs")
	return fmt.Sprintf("postgres://%s@%s:%s/%s", user, host, port, db)
}

func main() {
	apiBase := envOrDefault("STRUCTS_API_URL", "http://structsd:1317")
	pageLimit, _ := strconv.Atoi(envOrDefault("PAGE_LIMIT", "10000"))
	if pageLimit <= 0 {
		pageLimit = 10000
	}
	dbURL := buildDatabaseURL()

	rootCtx := context.Background()

	pool, err := pgxpool.New(rootCtx, dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(rootCtx); err != nil {
		log.Fatalf("Database ping failed: %v", err)
	}

	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	start := time.Now()
	log.Println("Updating Structs DB Cache based on chain data")

	g, ctx := errgroup.WithContext(rootCtx)

	for _, entity := range entities {
		entity := entity
		g.Go(func() error {
			return processEntity(ctx, httpClient, pool, apiBase, pageLimit, entity)
		})
	}

	if err := g.Wait(); err != nil {
		log.Fatalf("Cache update failed: %v", err)
	}

	if _, err := pool.Exec(rootCtx, "TRUNCATE cache.attributes_tmp"); err != nil {
		log.Printf("Warning: failed to truncate attributes_tmp: %v", err)
	}

	log.Printf("Cache update completed in %s", time.Since(start).Round(time.Millisecond))
}

func processEntity(ctx context.Context, client *http.Client, pool *pgxpool.Pool, apiBase string, pageLimit int, entity EntityConfig) error {
	items, err := fetchAllPages(ctx, client, apiBase, pageLimit, entity)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", entity.Endpoint, err)
	}

	if len(items) == 0 {
		log.Printf("%-30s  0 items (skipped)", entity.Endpoint)
		return nil
	}

	if err := bulkInsert(ctx, pool, entity.CompositeKey, items); err != nil {
		return fmt.Errorf("insert %s: %w", entity.Endpoint, err)
	}

	log.Printf("%-30s  %d items inserted", entity.Endpoint, len(items))
	return nil
}

func fetchAllPages(ctx context.Context, client *http.Client, apiBase string, pageLimit int, entity EntityConfig) ([]json.RawMessage, error) {
	var all []json.RawMessage
	var nextKey string

	for {
		u := fmt.Sprintf("%s/structs/%s?pagination.limit=%d", apiBase, entity.Endpoint, pageLimit)
		if nextKey != "" {
			u += "&pagination.key=" + url.QueryEscape(nextKey)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, u)
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}

		dataRaw, ok := raw[entity.DataKey]
		if !ok {
			break
		}

		var items []json.RawMessage
		if err := json.Unmarshal(dataRaw, &items); err != nil {
			return nil, fmt.Errorf("parse %s array: %w", entity.DataKey, err)
		}

		if len(items) == 0 {
			break
		}

		all = append(all, items...)

		paginationRaw, ok := raw["pagination"]
		if !ok {
			break
		}
		var pagination struct {
			NextKey *string `json:"next_key"`
		}
		if err := json.Unmarshal(paginationRaw, &pagination); err != nil || pagination.NextKey == nil || *pagination.NextKey == "" {
			break
		}
		nextKey = *pagination.NextKey
	}

	return all, nil
}

func bulkInsert(ctx context.Context, pool *pgxpool.Pool, compositeKey string, items []json.RawMessage) error {
	rows := make([][]any, len(items))
	for i, item := range items {
		rows[i] = []any{compositeKey, string(item)}
	}

	_, err := pool.CopyFrom(
		ctx,
		pgx.Identifier{"cache", "attributes_tmp"},
		[]string{"composite_key", "value"},
		pgx.CopyFromRows(rows),
	)
	return err
}

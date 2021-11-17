package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/estuary/connectors/sqlcapture"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// WriteWatermark writes a new random UUID into the 'watermarks' table and returns the UUID.
func (db *postgresDatabase) WriteWatermark(ctx context.Context) (string, error) {
	// Generate a watermark UUID
	var wm = uuid.New().String()

	var query = fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (slot TEXT PRIMARY KEY, watermark TEXT);", db.config.WatermarksTable)
	rows, err := db.conn.Query(ctx, query)
	if err != nil {
		return "", fmt.Errorf("error creating watermarks table: %w", err)
	}
	rows.Close()

	query = fmt.Sprintf(`INSERT INTO %s (slot, watermark) VALUES ($1,$2) ON CONFLICT (slot) DO UPDATE SET watermark = $2;`, db.config.WatermarksTable)
	rows, err = db.conn.Query(ctx, query, db.config.SlotName, wm)
	if err != nil {
		return "", fmt.Errorf("error upserting new watermark for slot %q: %w", db.config.SlotName, err)
	}
	rows.Close()

	logrus.WithField("watermark", wm).Debug("wrote watermark")
	return wm, nil
}

// WatermarksTable returns the name of the table to which WriteWatermarks writes UUIDs.
func (db *postgresDatabase) WatermarksTable() string {
	return db.config.WatermarksTable
}

// ScanTableChunk fetches a chunk of rows from the specified table, resuming from the provided
// `resumeKey` if non-nil.
func (db *postgresDatabase) ScanTableChunk(ctx context.Context, streamID string, keyColumns []string, resumeKey []interface{}) ([]sqlcapture.ChangeEvent, error) {
	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		var keyJSON, err = json.Marshal(resumeKey)
		if err != nil {
			logrus.WithField("err", err).Warn("error marshalling debug JSON")
		}
		logrus.WithFields(logrus.Fields{
			"streamID":   streamID,
			"keyColumns": keyColumns,
			"resumeKey":  string(keyJSON),
		}).Debug("scanning table chunk")
	}

	// Split "public.foo" tableID into "public" schema and "foo" table name
	var parts = strings.SplitN(streamID, ".", 2)
	var schemaName, tableName = parts[0], parts[1]

	// Build and execute a query to fetch the next `backfillChunkSize` rows from the database
	var query = buildScanQuery(resumeKey == nil, keyColumns, schemaName, tableName)
	logrus.WithField("query", query).WithField("args", resumeKey).Debug("executing query")
	rows, err := db.conn.Query(ctx, query, resumeKey...)
	if err != nil {
		return nil, fmt.Errorf("unable to execute query %q: %w", query, err)
	}
	defer rows.Close()

	// Process the results into `changeEvent` structs and return them
	var cols = rows.FieldDescriptions()
	var events []sqlcapture.ChangeEvent
	for rows.Next() {
		// Scan the row values and copy into the equivalent map
		var vals, err = rows.Values()
		if err != nil {
			return nil, fmt.Errorf("unable to get row values: %w", err)
		}
		var fields = make(map[string]interface{})
		for idx := range cols {
			fields[string(cols[idx].Name)] = vals[idx]
		}

		events = append(events, sqlcapture.ChangeEvent{
			Type:      "Insert",
			Namespace: schemaName,
			Table:     tableName,
			Fields:    fields,
		})
	}
	return events, nil
}

// backfillChunkSize controls how many rows will be read from the database in a
// single query. In normal use it acts like a constant, it's just a variable here
// so that it can be lowered in tests to exercise chunking behavior more easily.
var backfillChunkSize = 4096

func buildScanQuery(start bool, keyColumns []string, schemaName, tableName string) string {
	// Construct strings like `(foo, bar, baz)` and `($1, $2, $3)` for use in the query
	var pkey, args string
	for idx, colName := range keyColumns {
		if idx > 0 {
			pkey += ", "
			args += ", "
		}
		pkey += colName
		args += fmt.Sprintf("$%d", idx+1)
	}

	// Construct the query itself
	var query = new(strings.Builder)
	fmt.Fprintf(query, "SELECT * FROM %s.%s", schemaName, tableName)
	if !start {
		fmt.Fprintf(query, " WHERE (%s) > (%s)", pkey, args)
	}
	fmt.Fprintf(query, " ORDER BY (%s)", pkey)
	fmt.Fprintf(query, " LIMIT %d;", backfillChunkSize)
	return query.String()
}

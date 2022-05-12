package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
)

type ExternalStorage struct {
	ExternalDataConfig *bigquery.ExternalDataConfig
	object             *storage.ObjectHandle
	writer             *storage.Writer
	buffer             *bufio.Writer
	encoder            *json.Encoder
}

func NewExternalStorage(ctx context.Context, binding *Binding, path string) *ExternalStorage {
	obj := binding.bucket.Object(path)
	edc := &bigquery.ExternalDataConfig{
		SourceFormat: bigquery.JSON,
		Schema:       binding.Table.Schema,
		SourceURIs:   []string{fmt.Sprintf("gs://%s/%s", obj.BucketName(), obj.ObjectName())},
	}

	binding.bucket.Object(path)
	w := obj.NewWriter(ctx)
	b := bufio.NewWriter(w)

	e := json.NewEncoder(b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "")

	writer := &ExternalStorage{
		ExternalDataConfig: edc,
		object:             obj,
		writer:             w,
		buffer:             b,
		encoder:            e,
	}

	return writer
}

func (es *ExternalStorage) Store(doc map[string]interface{}) error {
	err := es.encoder.Encode(doc)

	return err
}

// After commit has returend, the writer cannot be written to
// as the underlying objects are closed.
// This method will make the writer as committed, even if an
// error occurred while comitting the data. This is so it tells the binding
// that it's not safe to write data to this writer anymore.
func (es *ExternalStorage) Commit(ctx context.Context) error {

	var err error
	if err = es.buffer.Flush(); err != nil {
		return fmt.Errorf("flushing the buffer: %w", err)
	}

	if err = es.writer.Close(); err != nil {
		return fmt.Errorf("closing the writer to cloud storage: %w", err)
	}

	return nil
}

// Delete the object associated to this Writer. Once deleted, the file
// cannot be recovered. This is a destructive operation that should only
// happen once the underlying data is written to bigquery. Otherwise, it could result
// in data loss.
func (es *ExternalStorage) Destroy(ctx context.Context) error {
	return es.object.Delete(ctx)
}

func randomString() string {
	id, err := uuid.NewRandom()
	if err != nil {
		panic(fmt.Errorf("generating UUID: %w", err))
	}

	return id.String()
}
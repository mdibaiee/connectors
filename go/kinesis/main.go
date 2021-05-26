package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/estuary/connectors/go/protocol"
	log "github.com/sirupsen/logrus"
)

func main() {
	protocol.RunMain(spec, doCheck, doDiscover, doRead)
}

// TODO: update docs link to kinesis connector-specific docs after they are written
var spec = protocol.Spec{
	SupportsIncremental:           true,
	SupportedDestinationSyncModes: protocol.AllDestinationSyncModes,
	ConnectionSpecification:       configJSONSchema,
}

func doCheck(args protocol.CheckCmd) error {
	var result = &protocol.ConnectionStatus{
		Status: protocol.StatusSucceeded,
	}
	var _, err = tryListingStreams(args.ConfigFile)
	if err != nil {
		result.Status = protocol.StatusFailed
		result.Message = err.Error()
	}
	return protocol.NewStdoutEncoder().Encode(protocol.Message{
		Type:             protocol.MessageTypeConnectionStatus,
		ConnectionStatus: result,
	})
}

func tryListingStreams(configFile protocol.ConfigFile) ([]string, error) {
	var _, client, err = parseConfigAndConnect(configFile)
	if err != nil {
		return nil, err
	}
	var ctx = context.Background()
	return listAllStreams(ctx, client)
}

func doDiscover(args protocol.DiscoverCmd) error {
	var catalog, err = discoverCatalog(args.ConfigFile)
	if err != nil {
		return err
	}
	log.Infof("Discover completed with %d streams", len(catalog.Streams))
	var encoder = protocol.NewStdoutEncoder()
	return encoder.Encode(catalog)
}

func discoverCatalog(config protocol.ConfigFile) (*protocol.Catalog, error) {
	var _, client, err = parseConfigAndConnect(config)
	if err != nil {
		return nil, err
	}
	var ctx = context.Background()
	streamNames, err := listAllStreams(ctx, client)

	var schema = protocol.UnknownSchema()

	var catalog = &protocol.Catalog{
		Streams: make([]protocol.Stream, len(streamNames)),
	}
	for i, name := range streamNames {
		catalog.Streams[i] = protocol.Stream{
			Name:                name,
			JSONSchema:          schema,
			SupportedSyncModes:  []protocol.SyncMode{protocol.SyncModeIncremental},
			SourceDefinedCursor: true,
		}
	}
	return catalog, nil
}

func put(state *protocol.State, source *kinesisSource, sequenceNumber string) {
	var streamMap, ok = state.Data[source.stream]
	if !ok {
		streamMap = make(map[string]interface{})
		state.Data[source.stream] = streamMap
	}
	streamMap.(map[string]interface{})[source.shardID] = sequenceNumber
}

func copyStreamState(state *protocol.Message, stream string) (map[string]string, error) {
	var dest = make(map[string]string)
	// Is there an entry for this stream
	if ss, ok := state.State.Data[stream]; ok {
		// Does the entry for this stream have the right type
		if typedSS, ok := ss.(map[string]interface{}); ok {
			for k, v := range typedSS {
				if vstr, ok := v.(string); ok {
					dest[k] = vstr
				} else {
					return nil, fmt.Errorf("found a non-string value in state map for stream: '%s'", stream)
				}
			}
		} else {
			return nil, fmt.Errorf("invalid state for stream '%s', expected values to be maps of string to string", stream)
		}
	}
	return dest, nil
}

func doRead(args protocol.ReadCmd) error {
	var config, client, err = parseConfigAndConnect(args.ConfigFile)
	if err != nil {
		return err
	}
	var catalog protocol.ConfiguredCatalog
	err = args.CatalogFile.Parse(&catalog)
	if err != nil {
		return fmt.Errorf("parsing configured catalog: %w", err)
	}
	err = catalog.Validate()
	if err != nil {
		return fmt.Errorf("configured catalog is invalid: %w", err)
	}

	var stateMessage = protocol.Message{
		Type: protocol.MessageTypeState,
		State: &protocol.State{
			Data: make(map[string]interface{}),
		},
	}
	err = args.StateFile.Parse(&stateMessage.State.Data)
	if err != nil {
		return fmt.Errorf("parsing state file: %w", err)
	}

	var dataCh = make(chan readResult, 8)
	var ctx, cancelFunc = context.WithCancel(context.Background())

	log.WithField("streamCount", len(catalog.Streams)).Info("Starting to read stream(s)")

	for _, stream := range catalog.Streams {
		streamState, err := copyStreamState(&stateMessage, stream.Stream.Name)
		if err != nil {
			cancelFunc()
			return fmt.Errorf("invalid state for stream %s: %w", stream.Stream.Name, err)
		}
		go readStream(ctx, config, client, stream.Stream.Name, streamState, dataCh)
	}

	// We'll re-use this same message instance for all records we print
	var recordMessage = protocol.Message{
		Type:   protocol.MessageTypeRecord,
		Record: &protocol.Record{},
	}
	// We're all set to start printing data to stdout
	var encoder = json.NewEncoder(os.Stdout)
	for {
		var next = <-dataCh
		if next.Error != nil {
			// time to bail
			var errMessage = protocol.NewLogMessage(protocol.LogLevelFatal, "read failed due to error: %v", next.Error)
			// Printing the error may fail, but we'll ignore that error and return the original
			_ = encoder.Encode(errMessage)
			cancelFunc()
			return next.Error
		} else {
			recordMessage.Record.Stream = next.Source.stream
			for _, record := range next.Records {
				recordMessage.Record.Data = record
				recordMessage.Record.EmittedAt = time.Now().UTC().UnixNano() / int64(time.Millisecond)
				var err = encoder.Encode(recordMessage)
				if err != nil {
					cancelFunc()
					return err
				}
			}
			put(stateMessage.State, next.Source, next.SequenceNumber)
			err = encoder.Encode(stateMessage)
			if err != nil {
				cancelFunc()
				return err
			}
		}
	}
}

func parseConfigAndConnect(configFile protocol.ConfigFile) (config Config, client *kinesis.Kinesis, err error) {
	err = configFile.ConfigFile.Parse(&config)
	if err != nil {
		err = fmt.Errorf("parsing config file: %w", err)
		return
	}
	// If the partition range was not included in the configuration, then we'll assume the full
	// range.
	if config.PartitionRange == nil {
		log.Info("Assuming full partition range since no partitionRange was included in the configuration")
		var fullRange = protocol.NewFullPartitionRange()
		config.PartitionRange = &fullRange
	}
	client, err = connect(&config)
	if err != nil {
		err = fmt.Errorf("failed to connect: %w", err)
	}
	return
}
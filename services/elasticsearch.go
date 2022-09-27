package services

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Velocidex/ordereddict"
	"github.com/aws/aws-sdk-go/aws/session"

	opensearch "github.com/opensearch-project/opensearch-go"
	opensearchapi "github.com/opensearch-project/opensearch-go/opensearchapi"
	"github.com/opensearch-project/opensearch-go/opensearchutil"
	requestsigner "github.com/opensearch-project/opensearch-go/signer/aws"

	"www.velocidex.com/golang/cloudvelo/config"
	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/crypto"
	"www.velocidex.com/golang/velociraptor/json"
	"www.velocidex.com/golang/velociraptor/logging"
	"www.velocidex.com/golang/velociraptor/utils"
)

const (
	AsyncDelete = false
	SyncDelete  = true

	NoSortField = ""
)

var (
	mu             sync.Mutex
	gElasticClient *opensearch.Client
	TRUE           = true
	True           = "true"

	logger *logging.LogContext

	bulk_indexer opensearchutil.BulkIndexer
)

// The logger is normally installed in the start up sequence with
// SetDebugLogger() below.
func Debug(format string, args ...interface{}) func() {
	start := time.Now()
	return func() {
		if logger != nil {
			args = append(args, time.Now().Sub(start))
			logger.Debug(format+" in %v", args...)
		}
	}
}

type IndexInfo struct {
	Index string `json:"index"`
}

func ListIndexes(ctx context.Context) ([]string, error) {
	client, err := GetElasticClient()
	if err != nil {
		return nil, err
	}

	res, err := opensearchapi.CatIndicesRequest{
		Format: "json",
	}.Do(ctx, client)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	indexes := []*IndexInfo{}
	err = json.Unmarshal(data, &indexes)
	if err != nil {
		return nil, err
	}

	results := make([]string, len(indexes))
	for _, i := range indexes {
		results = append(results, i.Index)
	}

	return results, nil

}

func GetIndex(org_id, index string) string {
	if org_id == "root" {
		org_id = ""
	}

	if org_id == "" {
		return index
	}
	return fmt.Sprintf(
		"%s_%s", strings.ToLower(org_id), index)
}

func DeleteDocument(
	ctx context.Context, org_id, index string, id string, sync bool) error {
	defer Debug("DeleteDocument %v", id)()
	client, err := GetElasticClient()
	if err != nil {
		return err
	}

	res, err := opensearchapi.DeleteRequest{
		Index:      GetIndex(org_id, index),
		DocumentID: id,
	}.Do(ctx, client)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if sync {
		res, err = opensearchapi.IndicesRefreshRequest{
			Index: []string{GetIndex(org_id, index)},
		}.Do(ctx, client)
		defer res.Body.Close()
	}

	return err
}

// Should be called to force the index to synchronize.
func FlushIndex(
	ctx context.Context, org_id, index string) error {
	client, err := GetElasticClient()
	if err != nil {
		return err
	}

	res, err := opensearchapi.IndicesRefreshRequest{
		Index: []string{GetIndex(org_id, index)},
	}.Do(ctx, client)

	defer res.Body.Close()

	return err
}

func UpdateIndex(
	ctx context.Context, org_id, index, id string, query string) error {
	defer Debug("UpdateIndex %v %v", index, id)()
	return retry(func() error {
		return _UpdateIndex(ctx, org_id, index, id, query)
	})
}

func _UpdateIndex(
	ctx context.Context, org_id, index, id string, query string) error {
	client, err := GetElasticClient()
	if err != nil {
		return err
	}

	es_req := opensearchapi.UpdateRequest{
		Index:      GetIndex(org_id, index),
		DocumentID: id,
		Body:       strings.NewReader(query),
		Refresh:    "true",
	}

	res, err := es_req.Do(ctx, client)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	// All is well we dont need to parse the results
	if !res.IsError() {
		return nil
	}

	response := ordereddict.NewDict()
	err = response.UnmarshalJSON(data)
	if err != nil {
		return err
	}

	return makeElasticError(response)
}

func UpdateByQuery(
	ctx context.Context, org_id, index string, query string) error {
	client, err := GetElasticClient()
	if err != nil {
		return err
	}

	es_req := opensearchapi.UpdateByQueryRequest{
		Index:   []string{GetIndex(org_id, index)},
		Body:    strings.NewReader(query),
		Refresh: &TRUE,
	}

	res, err := es_req.Do(ctx, client)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	// All is well we dont need to parse the results
	if !res.IsError() {
		return nil
	}

	response := ordereddict.NewDict()
	err = response.UnmarshalJSON(data)
	if err != nil {
		return err
	}

	return makeElasticError(response)
}

func SetElasticIndexAsync(org_id, index, id string, record interface{}) error {
	defer Debug("SetElasticIndexAsync %v %v", index, id)()
	mu.Lock()
	l_bulk_indexer := bulk_indexer
	mu.Unlock()

	serialized := json.MustMarshalString(record)

	// Add with background context which might outlive our caller.
	return l_bulk_indexer.Add(context.Background(),
		opensearchutil.BulkIndexerItem{
			Index:      GetIndex(org_id, index),
			Action:     "index",
			DocumentID: id,
			Body:       strings.NewReader(serialized),
		})
}

func SetElasticIndex(ctx context.Context,
	org_id, index, id string, record interface{}) error {
	defer Debug("SetElasticIndex %v %v", index, id)()
	return retry(func() error {
		return _SetElasticIndex(ctx, org_id, index, id, record)
	})
}

func _SetElasticIndex(
	ctx context.Context, org_id, index, id string, record interface{}) error {
	serialized := json.MustMarshalIndent(record)
	client, err := GetElasticClient()
	if err != nil {
		return err
	}

	es_req := opensearchapi.IndexRequest{
		Index:      GetIndex(org_id, index),
		DocumentID: id,
		Body:       bytes.NewReader(serialized),
		Refresh:    "true",
	}

	res, err := es_req.Do(ctx, client)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	// All is well we dont need to parse the results
	if !res.IsError() {
		return nil
	}

	response := ordereddict.NewDict()
	err = response.UnmarshalJSON(data)
	if err != nil {
		return err
	}

	return makeElasticError(response)
}

type _ElasticHit struct {
	Index  string          `json:"_index"`
	Source json.RawMessage `json:"_source"`
	Id     string          `json:"_id"`
}

type _ElasticHits struct {
	Hits []_ElasticHit `json:"hits"`
}

type _AggBucket struct {
	Key   interface{} `json:"key"`
	Count int         `json:"doc_count"`
}

type _AggResults struct {
	Buckets []_AggBucket `json:"buckets"`
	Value   interface{}  `json:"value"`
}

type _ElasticAgg struct {
	Results _AggResults `json:"genres"`
}

type _ElasticResponse struct {
	Took         int          `json:"took"`
	Hits         _ElasticHits `json:"hits"`
	Aggregations _ElasticAgg  `json:"aggregations"`
}

func GetElasticRecord(
	ctx context.Context, org_id, index, id string) (json.RawMessage, error) {
	defer Debug("GetElasticRecord %v %v", index, id)()
	client, err := GetElasticClient()
	if err != nil {
		return nil, err
	}

	res, err := opensearchapi.GetRequest{
		Index:      GetIndex(org_id, index),
		DocumentID: id,
	}.Do(ctx, client)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	// All is well we dont need to parse the results
	if !res.IsError() {
		hit := &_ElasticHit{}
		err := json.Unmarshal(data, hit)
		return hit.Source, err
	}

	response := ordereddict.NewDict()
	err = response.UnmarshalJSON(data)
	if err != nil {
		return nil, err
	}

	found_any, pres := response.Get("found")
	if pres {
		found, ok := found_any.(bool)
		if ok && !found {
			return nil, os.ErrNotExist
		}
	}

	return nil, makeElasticError(response)
}

// Automatically take care of paging by returning a channel.  Query
// should be a JSON query **without** a sorting clause, or "size"
// clause.
// This function will modify the query to add a sorting column and
// automatically apply the search_after to page through the
// results. Currently we do not take a point in time snapshot so
// results are approximate.
func QueryChan(
	ctx context.Context,
	config_obj *config_proto.Config,
	page_size int,
	org_id, index, query, sort_field string) (
	chan json.RawMessage, error) {

	defer Debug("QueryChan %v", index)()

	output_chan := make(chan json.RawMessage)

	query = strings.TrimSpace(query)
	var part_query string
	if sort_field != "" {
		part_query = json.Format(`{"sort":[{%q: "asc"}], "size":%q,`,
			sort_field, page_size)
	} else {
		part_query = json.Format(`{"size":%q,`, page_size)
	}
	part_query += query[1:]

	part, err := QueryElasticRaw(ctx, org_id, index, part_query)
	if err != nil {
		return nil, err
	}

	var search_after interface{}
	var pres bool

	go func() {
		defer close(output_chan)

		for {
			if len(part) == 0 {
				return
			}
			for idx, p := range part {
				select {
				case <-ctx.Done():
					return
				case output_chan <- p:
				}

				// On the last row we look at the result so we can get
				// the next part.
				if idx == len(part)-1 {
					row := ordereddict.NewDict()
					err := row.UnmarshalJSON(p)
					if err != nil {
						logger := logging.GetLogger(config_obj,
							&logging.FrontendComponent)
						logger.Error("QueryChan: %v", err)
						return
					}

					search_after, pres = row.Get(sort_field)
					if !pres {
						return
					}
				}
			}

			// Form the next query using the search_after value.
			part_query := json.Format(`
{"sort":[{%q: "asc"}], "size":%q,"search_after": [%q],`,
				sort_field, page_size, search_after) + query[1:]

			part, err = QueryElasticRaw(ctx, org_id, index, part_query)
			if err != nil {
				logger := logging.GetLogger(config_obj,
					&logging.FrontendComponent)
				logger.Error("QueryChan: %v", err)
				return
			}
		}
	}()

	return output_chan, nil
}

func DeleteByQuery(
	ctx context.Context, org_id, index, query string) error {
	client, err := GetElasticClient()
	if err != nil {
		return err
	}

	res, err := opensearchapi.DeleteByQueryRequest{
		Index:   []string{GetIndex(org_id, index)},
		Body:    strings.NewReader(query),
		Refresh: &TRUE,
	}.Do(ctx, client)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	// All is well we dont need to parse the results
	if !res.IsError() {
		return nil
	}

	response := ordereddict.NewDict()
	err = response.UnmarshalJSON(data)
	if err != nil {
		return err
	}

	return makeElasticError(response)
}

func QueryElasticAggregations(
	ctx context.Context, org_id, index, query string) ([]string, error) {

	defer Debug("QueryElasticAggregations %v", index)()

	es, err := GetElasticClient()
	if err != nil {
		return nil, err
	}
	res, err := es.Search(
		es.Search.WithContext(ctx),
		es.Search.WithIndex(GetIndex(org_id, index)),
		es.Search.WithBody(strings.NewReader(query)),
		es.Search.WithPretty(),
	)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	// There was an error so we need to relay it
	if res.IsError() {
		response := ordereddict.NewDict()
		err = response.UnmarshalJSON(data)
		if err != nil {
			return nil, err
		}

		return nil, makeElasticError(response)
	}

	parsed := &_ElasticResponse{}
	err = json.Unmarshal(data, &parsed)
	if err != nil {
		return nil, err
	}

	var results []string
	// Handle value aggregates
	if !utils.IsNil(parsed.Aggregations.Results.Value) {
		results = append(results, to_string(parsed.Aggregations.Results.Value))
		return results, nil
	}

	for _, hit := range parsed.Aggregations.Results.Buckets {
		results = append(results, to_string(hit.Key))
	}

	return results, nil
}

func to_string(a interface{}) string {
	switch t := a.(type) {
	case string:
		return t

	default:
		return string(json.MustMarshalIndent(a))
	}
}

func QueryElasticRaw(
	ctx context.Context,
	org_id, index, query string) ([]json.RawMessage, error) {

	defer Debug("QueryElasticRaw %v", index)()

	es, err := GetElasticClient()
	if err != nil {
		return nil, err
	}
	res, err := es.Search(
		es.Search.WithContext(ctx),
		es.Search.WithIndex(GetIndex(org_id, index)),
		es.Search.WithBody(strings.NewReader(query)),
		es.Search.WithPretty(),
	)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	// There was an error so we need to relay it
	if res.IsError() {
		response := ordereddict.NewDict()
		err = response.UnmarshalJSON(data)
		if err != nil {
			return nil, err
		}

		return nil, makeElasticError(response)
	}

	parsed := &_ElasticResponse{}
	err = json.Unmarshal(data, &parsed)
	if err != nil {
		return nil, err
	}

	var results []json.RawMessage
	for _, hit := range parsed.Hits.Hits {
		results = append(results, hit.Source)
	}

	return results, nil
}

// Return only Ids of matching documents.
func QueryElasticIds(
	ctx context.Context,
	org_id, index, query string) ([]string, error) {

	es, err := GetElasticClient()
	if err != nil {
		return nil, err
	}
	res, err := es.Search(
		es.Search.WithContext(ctx),
		es.Search.WithIndex(GetIndex(org_id, index)),
		es.Search.WithBody(strings.NewReader(query)),
		es.Search.WithPretty(),
	)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	// There was an error so we need to relay it
	if res.IsError() {
		response := ordereddict.NewDict()
		err = response.UnmarshalJSON(data)
		if err != nil {
			return nil, err
		}

		return nil, makeElasticError(response)
	}

	parsed := &_ElasticResponse{}
	err = json.Unmarshal(data, &parsed)
	if err != nil {
		return nil, err
	}

	var results []string
	for _, hit := range parsed.Hits.Hits {
		results = append(results, hit.Id)
	}

	return results, nil
}

type Result struct {
	JSON json.RawMessage
	Id   string
}

func QueryElastic(
	ctx context.Context,
	org_id, index, query string) ([]Result, error) {

	es, err := GetElasticClient()
	if err != nil {
		return nil, err
	}
	res, err := es.Search(
		es.Search.WithContext(ctx),
		es.Search.WithIndex(GetIndex(org_id, index)),
		es.Search.WithBody(strings.NewReader(query)),
		es.Search.WithPretty(),
	)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	// There was an error so we need to relay it
	if res.IsError() {
		response := ordereddict.NewDict()
		err = response.UnmarshalJSON(data)
		if err != nil {
			return nil, err
		}

		return nil, makeElasticError(response)
	}

	parsed := &_ElasticResponse{}
	err = json.Unmarshal(data, &parsed)
	if err != nil {
		return nil, err
	}

	var results []Result
	for _, hit := range parsed.Hits.Hits {
		results = append(results, Result{
			JSON: hit.Source,
			Id:   hit.Id,
		})
	}

	return results, nil
}

func GetElasticClient() (*opensearch.Client, error) {
	mu.Lock()
	defer mu.Unlock()

	if gElasticClient == nil {
		return nil, errors.New("Elastic configuration not initialized")
	}

	return gElasticClient, nil
}

func SetElasticClient(c *opensearch.Client) {
	mu.Lock()
	defer mu.Unlock()

	gElasticClient = c
}

func SetDebugLogger(config_obj *config_proto.Config) {
	mu.Lock()
	defer mu.Unlock()

	logger = logging.GetLogger(config_obj, &logging.FrontendComponent)
}

func StartElasticSearchService(config_obj *config.Config) error {
	cfg := opensearch.Config{
		Addresses: config_obj.Cloud.Addresses,
	}

	CA_Pool := x509.NewCertPool()
	crypto.AddPublicRoots(CA_Pool)

	if config_obj.Cloud.RootCerts != "" &&
		!CA_Pool.AppendCertsFromPEM([]byte(config_obj.Cloud.RootCerts)) {
		return errors.New("cloud ingestion: Unable to add root certs")
	}

	cfg.Transport = &http.Transport{
		MaxIdleConnsPerHost:   10,
		ResponseHeaderTimeout: 100 * time.Second,
		TLSClientConfig: &tls.Config{
			ClientSessionCache: tls.NewLRUClientSessionCache(100),
			RootCAs:            CA_Pool,
			InsecureSkipVerify: config_obj.Cloud.DisableSSLSecurity,
		},
	}

	if config_obj.Cloud.Username != "" && config_obj.Cloud.Password != "" {
		cfg.Username = config_obj.Cloud.Username
		cfg.Password = config_obj.Cloud.Password
	} else {
		signer, err := requestsigner.NewSigner(session.Options{SharedConfigState: session.SharedConfigEnable})
		if err != nil {
			return err
		}
		cfg.Signer = signer
	}

	client, err := opensearch.NewClient(cfg)
	if err != nil {
		return err
	}

	// Fetch info immediately to verify that we can actually connect
	// to the server.
	res, err := client.Info()
	if err != nil {
		return err
	}

	defer res.Body.Close()

	// Set the global elastic client
	SetElasticClient(client)

	return nil
}

func makeElasticError(response *ordereddict.Dict) error {
	err_type := utils.GetString(response, "error.type")
	err_reason := utils.GetString(response, "error.reason")
	if false && err_type != "" && err_reason != "" {
		return fmt.Errorf("Elastic Error: %v: %v", err_type, err_reason)
	}

	return fmt.Errorf("Elastic Error: %v", response)
}

// Convert the item into a unique document ID - This is needed when
// the item can be longer than the maximum 512 bytes.
func MakeId(item string) string {
	hash := sha1.Sum([]byte(item))
	return hex.EncodeToString(hash[:])
}

func StartBulkIndexService(
	ctx context.Context,
	wg *sync.WaitGroup,
	config_obj *config.Config) error {
	elastic_client, err := GetElasticClient()
	if err != nil {
		return err
	}

	logger := logging.GetLogger(
		config_obj.VeloConf(), &logging.FrontendComponent)

	new_bulk_indexer, err := opensearchutil.NewBulkIndexer(
		opensearchutil.BulkIndexerConfig{
			Client: elastic_client,
			OnError: func(ctx context.Context, err error) {
				if err != nil {
					logger.Error("BulkIndexerConfig: %v", err)
				}
			},
		})
	if err != nil {
		return err
	}

	mu.Lock()
	bulk_indexer = new_bulk_indexer
	mu.Unlock()

	// Ensure we flush the indexer before we exit.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()

		subctx, cancel := context.WithTimeout(context.Background(),
			30*time.Second)
		defer cancel()

		bulk_indexer.Close(subctx)
	}()

	return err
}
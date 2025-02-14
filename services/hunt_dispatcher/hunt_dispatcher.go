package hunt_dispatcher

import (
	"context"
	"errors"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	cvelo_services "www.velocidex.com/golang/cloudvelo/services"
	api_proto "www.velocidex.com/golang/velociraptor/api/proto"
	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/json"
	"www.velocidex.com/golang/velociraptor/services"
)

type HuntEntry struct {
	HuntId    string `json:"hunt_id"`
	Timestamp int64  `json:"timestamp"`
	Expires   uint64 `json:"expires"`
	Scheduled uint64 `json:"scheduled"`
	Completed uint64 `json:"completed"`
	Errors    uint64 `json:"errors"`
	Hunt      string `json:"hunt"`
	State     string `json:"state"`
	DocType   string `json:"doc_type"`
}

func (self *HuntEntry) GetHunt() (*api_proto.Hunt, error) {

	// Protobufs must only be marshalled/unmarshalled using protojson
	// because they are not compatible with the standard json package.
	hunt_info := &api_proto.Hunt{}
	err := protojson.Unmarshal([]byte(self.Hunt), hunt_info)
	if err != nil {
		return nil, err
	}

	hunt_info.Stats = &api_proto.HuntStats{
		TotalClientsScheduled:   self.Scheduled,
		TotalClientsWithResults: self.Completed,
		TotalClientsWithErrors:  self.Errors,
	}
	switch self.State {
	case "PAUSED":
		hunt_info.State = api_proto.Hunt_PAUSED

	case "RUNNING":
		hunt_info.State = api_proto.Hunt_RUNNING

	case "STOPPED":
		hunt_info.State = api_proto.Hunt_STOPPED

	case "ARCHIVED":
		hunt_info.State = api_proto.Hunt_ARCHIVED
	}

	return hunt_info, nil
}

type HuntDispatcher struct {
	ctx        context.Context
	config_obj *config_proto.Config
}

// TODO: Deprecated - remove.
func (self HuntDispatcher) ApplyFuncOnHunts(cb func(hunt *api_proto.Hunt) error) error {
	return errors.New("HuntDispatcher.ApplyFuncOnHunts Not implemented")
}

func (self HuntDispatcher) ApplyFuncOnHuntsWithOptions(
	ctx context.Context,
	options cvelo_services.HuntSearchOptions,
	cb func(hunt *api_proto.Hunt) error) error {

	sub_ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var query string
	switch options {
	case cvelo_services.AllHunts:
		query = getAllHunts
	case cvelo_services.OnlyRunningHunts:
		query = getAllActiveHunts
	default:
		return errors.New("HuntSearchOptions not supported")
	}

	out, err := cvelo_services.QueryChan(
		sub_ctx, self.config_obj, 1000, self.config_obj.OrgId,
		"persisted", query, "hunt_id")
	if err != nil {
		return err
	}

	for hit := range out {
		entry := &HuntEntry{}
		err := json.Unmarshal(hit, entry)
		if err != nil {
			return err
		}

		hunt_obj, err := entry.GetHunt()
		if err != nil {
			continue
		}

		// Allow the cb to cancel the query.
		err = cb(hunt_obj)
		if err != nil {
			return err
		}
	}

	return nil
}

func (self HuntDispatcher) GetLastTimestamp() uint64 {
	return 0
}

func (self HuntDispatcher) SetHunt(hunt *api_proto.Hunt) error {
	hunt_id := hunt.HuntId
	if hunt_id == "" {
		return errors.New("Invalid hunt")
	}

	serialized, err := protojson.Marshal(hunt)
	if err != nil {
		return err
	}

	record := &HuntEntry{
		HuntId:    hunt_id,
		Timestamp: int64(hunt.CreateTime / 1000000),
		Expires:   hunt.Expires,
		Hunt:      string(serialized),
		State:     hunt.State.String(),
		DocType:   "hunts",
	}

	if hunt.Stats != nil {
		record.Scheduled = hunt.Stats.TotalClientsScheduled
		record.Completed = hunt.Stats.TotalClientsWithResults
		record.Errors = hunt.Stats.TotalClientsWithErrors
	}

	return cvelo_services.SetElasticIndex(self.ctx,
		self.config_obj.OrgId,
		"persisted", hunt.HuntId,
		record)
}

func (self HuntDispatcher) GetHunt(hunt_id string) (*api_proto.Hunt, bool) {
	serialized, err := cvelo_services.GetElasticRecord(context.Background(),
		self.config_obj.OrgId, "persisted", hunt_id)
	if err != nil {
		return nil, false
	}

	hunt_entry := &HuntEntry{}
	err = json.Unmarshal(serialized, hunt_entry)
	if err != nil {
		return nil, false
	}

	hunt_info, err := hunt_entry.GetHunt()
	if err != nil {
		return nil, false
	}

	hunt_info.Stats.AvailableDownloads, _ = availableHuntDownloadFiles(
		self.config_obj, hunt_id)

	return hunt_info, true
}

func (self HuntDispatcher) MutateHunt(
	ctx context.Context,
	config_obj *config_proto.Config,
	mutation *api_proto.HuntMutation) error {
	return errors.New("HuntDispatcher.HuntMutation Not implemented")
}

func (self HuntDispatcher) Refresh(
	ctx context.Context,
	config_obj *config_proto.Config) error {
	return nil
}

func (self HuntDispatcher) Close(config_obj *config_proto.Config) {}

// TODO add sort and from/size clause
const (
	getAllHuntsQuery = `
{
    "query": {
      "bool": {
        "must": [{"match": {
                    "doc_type": "hunts"
                 }}]
      }
    },"sort": [{
    "hunt_id": {"order": "desc", "unmapped_type": "keyword"}
}],
 "from": %q, "size": %q
}
`
	getAllActiveHunts = `
{
    "query": {
        "bool": {
            "must": [
                {
                    "match": {
                        "doc_type": "hunts"
                    }
                },
                {
                    "match": {
                        "state": "RUNNING"
                    }
                }
            ]
        }
    }
}
`
	getAllHunts = `
{
    "query": {
        "bool": {
            "must": [
                {
                    "match": {
                        "doc_type": "hunts"
                    }
                }
            ]
        }
    }
}
`
)

// TODO: Deprecated...
func (self HuntDispatcher) ListHunts(
	ctx context.Context, config_obj *config_proto.Config,
	in *api_proto.ListHuntsRequest) (
	*api_proto.ListHuntsResponse, error) {

	hits, _, err := cvelo_services.QueryElasticRaw(
		ctx, self.config_obj.OrgId,
		"persisted", json.Format(getAllHuntsQuery, in.Offset, in.Count))
	if err != nil {
		return nil, err
	}

	result := &api_proto.ListHuntsResponse{}
	for _, hit := range hits {
		entry := &HuntEntry{}
		err = json.Unmarshal(hit, entry)
		if err != nil {
			continue
		}

		hunt_info, err := entry.GetHunt()
		if err != nil {
			continue
		}

		if in.UserFilter != "" &&
			in.UserFilter != hunt_info.Creator {
			continue
		}

		if hunt_info.State != api_proto.Hunt_ARCHIVED {
			result.Items = append(result.Items, hunt_info)
		}
	}

	return result, nil
}

func NewHuntDispatcher(
	ctx context.Context,
	wg *sync.WaitGroup,
	config_obj *config_proto.Config) (services.IHuntDispatcher, error) {
	service := &HuntDispatcher{
		ctx:        ctx,
		config_obj: config_obj,
	}

	return service, nil
}

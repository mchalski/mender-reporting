// Copyright 2021 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	es "github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/esapi"
	"github.com/elastic/go-elasticsearch/v7/esutil"
	"github.com/mendersoftware/go-lib-micro/identity"
	"github.com/mendersoftware/go-lib-micro/log"
	_ "github.com/mendersoftware/go-lib-micro/log"
	"github.com/pkg/errors"

	"github.com/mendersoftware/reporting/model"
)

type Store interface {
	IndexDevice(ctx context.Context, device *model.Device) error
	BulkIndexDevices(ctx context.Context, devices []*model.Device) error

	Search(ctx context.Context, query interface{}) (model.M, error)
	GetDevice(ctx context.Context, tenant, devid string) (*model.Device, error)
	UpdateDevice(ctx context.Context, tenantID, deviceID string, updateDev *model.Device) error
	Migrate(ctx context.Context) error
}

type StoreOption func(*store)

type store struct {
	addresses []string
	client    *es.Client
}

func NewStore(opts ...StoreOption) (Store, error) {
	store := &store{}
	for _, opt := range opts {
		opt(store)
	}

	cfg := es.Config{
		Addresses: store.addresses,
	}
	esClient, err := es.NewClient(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "invalid Elasticsearch configuration")
	}

	_, err = esClient.Ping()
	if err != nil {
		return nil, errors.Wrap(err, "unable to connect to Elasticsearch")
	}

	store.client = esClient
	return store, nil
}

func (s *store) IndexDevice(ctx context.Context, device *model.Device) error {
	req := esapi.IndexRequest{
		Index:      indexDevices + "-" + device.GetTenantID(),
		DocumentID: device.GetID(),
		Body:       esutil.NewJSONReader(device),
	}

	res, err := req.Do(ctx, s.client)
	if err != nil {
		return errors.Wrap(err, "failed to index")
	}
	defer res.Body.Close()

	return nil
}

type bulkAction struct {
	Index *bulkActionIndex `json:"index"`
}

type bulkActionIndex struct {
	ID    string `json:"_id"`
	Index string `json:"_index"`
}

func (s *store) BulkIndexDevices(ctx context.Context, devices []*model.Device) error {
	data := ""
	for _, device := range devices {
		actionJSON, err := json.Marshal(bulkAction{
			Index: &bulkActionIndex{
				ID:    device.GetID(),
				Index: indexDevices + "-" + device.GetTenantID(),
			},
		})
		if err != nil {
			return err
		}
		deviceJSON, err := json.Marshal(device)
		if err != nil {
			return err
		}
		data += string(actionJSON) + "\n" + string(deviceJSON) + "\n"

	}
	req := esapi.BulkRequest{
		Body: strings.NewReader(data),
	}
	res, err := req.Do(ctx, s.client)
	if err != nil {
		return errors.Wrap(err, "failed to bulk index")
	}
	defer res.Body.Close()

	return nil
}

func (s *store) Migrate(ctx context.Context) error {
	req := esapi.IndicesPutIndexTemplateRequest{
		Name: indexDevices,
		Body: strings.NewReader(indexDevicesTemplate),
	}

	res, err := req.Do(ctx, s.client)
	if err != nil {
		return errors.Wrap(err, "failed to put the index template")
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return errors.New("failed to set up the index template")
	}

	return nil
}

func (s *store) Search(ctx context.Context, query interface{}) (model.M, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(query); err != nil {
		return nil, err
	}

	id := identity.FromContext(ctx)

	resp, err := s.client.Search(
		s.client.Search.WithContext(ctx),
		s.client.Search.WithIndex("devices-"+id.Tenant),
		s.client.Search.WithBody(&buf),
		s.client.Search.WithTrackTotalHits(true),
	)
	defer resp.Body.Close()

	if err != nil {
		return nil, err
	}

	if resp.IsError() {
		return nil, errors.New(resp.String())
	}

	var ret map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&ret); err != nil {
		return nil, err
	}

	return ret, nil
}
func (s *store) GetDevice(ctx context.Context, tenant, devid string) (*model.Device, error) {
	//l := log.FromContext(ctx)

	id := identity.FromContext(ctx)

	req := esapi.GetRequest{
		Index:      indexDevices + "-" + id.Tenant,
		DocumentID: devid,
	}

	res, err := req.Do(ctx, s.client)
	defer res.Body.Close()

	switch {
	case err != nil:
		return nil, errors.Wrap(err, "failed to get device")
	case res.IsError():
		if res.StatusCode == http.StatusNotFound {
			return nil, nil
		} else {
			return nil, errors.New(fmt.Sprintf("failed to get device from ES, code %d", res.StatusCode))

		}
	}

	var storeRes map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&storeRes); err != nil {
		return nil, err
	}

	source, ok := storeRes["_source"].(map[string]interface{})
	if !ok {
		return nil, errors.New("can't process ES _source")
	}

	return model.NewDeviceFromEsSource(source)

}

func (s *store) UpdateDevice(ctx context.Context, tenantID, deviceID string, updateDev *model.Device) error {
	l := log.FromContext(ctx)

	id := identity.FromContext(ctx)

	body := map[string]interface{}{
		"doc": updateDev,
	}

	// DocumentType is _doc by default
	req := esapi.UpdateRequest{
		Index:      indexDevices + "-" + id.Tenant,
		DocumentID: deviceID,
		Body:       esutil.NewJSONReader(body),
	}

	res, err := req.Do(ctx, s.client)
	defer res.Body.Close()

	//DEBUG
	var esbody map[string]interface{}
	if err := json.NewDecoder(res.Body).Decode(&esbody); err != nil {
		return err
	}
	l.Debugf("ES update response %v", esbody)

	switch {
	case err != nil:
		return errors.Wrap(err, "failed to update device in ES")
	case res.IsError():
		return errors.New(fmt.Sprintf("failed to update device in ES, code %d", res.StatusCode))
	default:
		return nil
	}
}

func WithServerAddresses(addresses []string) StoreOption {
	return func(s *store) {
		s.addresses = addresses
	}
}

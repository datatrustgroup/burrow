// Copyright 2017 Monax Industries Limited
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"fmt"

	"github.com/tendermint/tendermint/rpc/lib/types"
)

type WebsocketClient interface {
	Send(ctx context.Context, request rpctypes.RPCRequest) error
}

func Subscribe(wsc WebsocketClient, eventId string) error {
	req, err := rpctypes.MapToRequest(fmt.Sprintf("wsclient_subscribe?eventId=%s", eventId),
		"subscribe", map[string]interface{}{"eventId": eventId})
	if err != nil {
		return err
	}
	return wsc.Send(context.Background(), req)

	//return wsc.Call(context.Background(), "subscribe",
	//	map[string]interface{}{"eventId": eventId})
}

func Unsubscribe(websocketClient WebsocketClient, subscriptionId string) error {
	req, err := rpctypes.MapToRequest(fmt.Sprintf("wsclient_unsubscribe?subId=%s", subscriptionId),
		"unsubscribe", map[string]interface{}{"subscriptionId": subscriptionId})
	if err != nil {
		return err
	}
	return websocketClient.Send(context.Background(), req)
}

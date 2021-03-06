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

package main

import (
	"fmt"

	. "github.com/hyperledger/burrow/logging/config"
)

// Dump an example logging configuration
func main() {
	loggingConfig := &LoggingConfig{
		RootSink: Sink().
			AddSinks(
				// Log everything to Stderr
				Sink().SetOutput(StderrOutput()),
				Sink().SetTransform(FilterTransform(ExcludeWhenAllMatch,
					"module", "p2p",
					"captured_logging_source", "tendermint_log15")).
					AddSinks(
						Sink().SetOutput(SyslogOutput("Burrow-network")),
						Sink().SetOutput(FileOutput("/var/log/burrow-network.log")),
					),
			),
	}
	fmt.Println(loggingConfig.RootTOMLString())
}

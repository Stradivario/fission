/*
Copyright 2016 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package app

import (
	"context"
	"net/http"
	"os"
	"strconv"

	"go.uber.org/zap"

	builder "github.com/fission/fission/pkg/builder"
	"github.com/fission/fission/pkg/utils/httpserver"
	"github.com/fission/fission/pkg/utils/manager"
)

// Usage: builder <shared volume path>
func Run(ctx context.Context, logger *zap.Logger, mgr manager.Interface, shareVolume string) {
	// Parse MAX_PARALLEL_BUILDS from env
	maxParallelBuilds := 1
	if envMaxParallelBuilds := os.Getenv("MAX_PARALLEL_BUILDS"); envMaxParallelBuilds != "" {
		if val, err := strconv.Atoi(envMaxParallelBuilds); err == nil {
			if val > 0 {
				maxParallelBuilds = val
			} else {
				logger.Warn("MAX_PARALLEL_BUILDS must be greater than 0, defaulting to 1", zap.Int("invalid_value", val))
			}
		} else {
			logger.Warn("Invalid MAX_PARALLEL_BUILDS value, defaulting to 1", zap.String("error", err.Error()))
		}
	}

	builder := builder.MakeBuilder(logger, shareVolume, maxParallelBuilds)
	mux := http.NewServeMux()
	mux.HandleFunc("/", builder.Handler)
	mux.HandleFunc("/clean", builder.Clean)
	mux.HandleFunc("/version", builder.VersionHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	httpserver.StartServer(ctx, logger, mgr, "builder", "8001", mux)
}

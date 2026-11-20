// SPDX-FileCopyrightText: The Fission Authors
//
// SPDX-License-Identifier: Apache-2.0

package fscache

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/utils/loggerfactory"
)

func fnMeta(gen int64, version string) *metav1.ObjectMeta {
	m := &metav1.ObjectMeta{Name: "nd", Namespace: "tenant", UID: types.UID("uid-1"), Generation: gen}
	if version != "" {
		m.Labels = map[string]string{fv1.FUNCTION_VERSION: version}
	}
	return m
}

// A newdeploy/container live Deployment is updated in place: after an update
// its cache entry must answer only the new Generation, so a FunctionVersion
// pinning the old Generation misses and gets its own -v<seq> objects instead
// of being served the live (updated) code.
func TestRekeyMovesLiveEntryToNewGeneration(t *testing.T) {
	t.Parallel()
	fsc := MakeFunctionServiceCache(loggerfactory.GetLogger())
	_, err := fsc.Add(FuncSvc{Name: "newdeploy-nd", Function: fnMeta(1, ""), Address: "nd.tenant:80"})
	require.NoError(t, err)

	live, err := fsc.GetLiveByFunctionUID("uid-1")
	require.NoError(t, err)
	require.NoError(t, fsc.Rekey(live, fnMeta(2, "")))

	_, err = fsc.GetByFunctionVersion(fnMeta(1, "nd-v1"))
	assert.Error(t, err, "the version pinned at the old Generation must miss")
	_, err = fsc.GetByFunction(fnMeta(1, ""))
	assert.Error(t, err, "nothing may stay under the old Generation")
	got, err := fsc.GetByFunctionVersion(fnMeta(2, ""))
	require.NoError(t, err, "the live lookup follows the new Generation")
	assert.Equal(t, "nd.tenant:80", got.Address)
}

// Even when (UID, Generation) match, an entry answers only its own version.
func TestGetByFunctionVersionRequiresSameVersion(t *testing.T) {
	t.Parallel()
	fsc := MakeFunctionServiceCache(loggerfactory.GetLogger())
	_, err := fsc.Add(FuncSvc{Name: "newdeploy-nd", Function: fnMeta(3, ""), Address: "live:80"})
	require.NoError(t, err)

	_, err = fsc.GetByFunctionVersion(fnMeta(3, "nd-v3"))
	assert.Error(t, err, "the live entry must not answer a versioned projection")
	got, err := fsc.GetByFunctionVersion(fnMeta(3, ""))
	require.NoError(t, err)
	assert.Equal(t, "live:80", got.Address)
}

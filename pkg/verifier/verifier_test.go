/*
Copyright 2025The Kubernetes Authors.

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

package verifier

import (
	"testing"

	fakesnapshot "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/fake"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes/fake"

	fakeSmsCR "github.com/kubernetes-csi/external-snapshot-metadata/client/clientset/versioned/fake"
	iter "github.com/kubernetes-csi/external-snapshot-metadata/pkg/iterator"
)

func TestValidateArgs(t *testing.T) {
	var err error
	args := Args{
		Args: iter.Args{
			Namespace:       "namespace",
			SnapshotName:    "snapshot",
			TokenExpirySecs: 60,
			MaxResults:      5,
			SANamespace:     "serviceAccountNamespace",
			SAName:          "serviceAccount",
			Emitter:         &VerifierEmitter{},
			Clients: iter.Clients{
				KubeClient:     fake.NewSimpleClientset(),
				SnapshotClient: fakesnapshot.NewSimpleClientset(),
				SmsCRClient:    fakeSmsCR.NewSimpleClientset(),
			},
		},
	}

	args.SourceDevicePath = "/dev/source"
	err = args.Validate()
	assert.Error(t, err)
	assert.ErrorIs(t, err, iter.ErrInvalidArgs)
	assert.ErrorContains(t, err, "Verify requires SourceDevicePath and TargetDevicePath")

	args.SourceDevicePath = ""
	args.TargetDevicePath = "/dev/target"
	err = args.Validate()
	assert.Error(t, err)
	assert.ErrorIs(t, err, iter.ErrInvalidArgs)
	assert.ErrorContains(t, err, "Verify requires SourceDevicePath and TargetDevicePath")

	args.SourceDevicePath = "/dev/source"
	args.TargetDevicePath = "/dev/target"
	err = args.Validate()
	assert.NoError(t, err)
}

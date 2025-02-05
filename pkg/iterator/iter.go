/*
Copyright 2024 The Kubernetes Authors.

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

package iterator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	grpcCreds "google.golang.org/grpc/credentials"
	authv1 "k8s.io/api/authentication/v1"
	apimetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	smsCRv1alpha1 "github.com/kubernetes-csi/external-snapshot-metadata/client/apis/snapshotmetadataservice/v1alpha1"
	"github.com/kubernetes-csi/external-snapshot-metadata/pkg/api"
)

var (
	ErrInvalidArgs = errors.New("invalid argument")
	ErrCACert      = errors.New("failed to add the SnapshotMetadataService CR CA certificate")
	ErrCancelled   = errors.New("enumeration cancelled")
)

const (
	DefaultTokenExpirySeconds = int64(600)

	// See "Service account tokens" in
	// https://kubernetes.io/docs/reference/access-authn-authz/authentication/.
	// It turns out that a service account name starts with a well defined prefix
	// that is guaranteed not to match any other user name.
	K8sServiceAccountUserNamePrefix = "system:serviceaccount:"
)

// GetSnapshotMetadata enumerates either the allocated blocks of a
// VolumeSnapshot object, or the blocks changed between a pair of
// VolumeSnapshot objects.
//
// Metadata is returned via an emitter interface specified in the
// invocation arguments. Iteration terminates on the first error
// encountered, or if requested by the emitter.
func GetSnapshotMetadata(ctx context.Context, args Args) error {
	if err := args.Validate(); err != nil {
		return err
	}

	return NewIterator(args).Run(ctx)
}

type Args struct {
	// Client interfaces are obtained from BuildClients.
	Clients

	// Emitter is an interface used to return metadata to the invoker.
	Emitter IteratorEmitter

	// Specify the namespace of the VolumeSnapshot objects.
	Namespace string

	// SnapshotName identifies a VolumeSnaphot.
	SnapshotName string

	// PrevSnapshotName is optional, and if specified will result in
	// enumeration of the changed blocks between the VolumeSnapshot
	// identified by it and that identified by the SnapshotName field.
	// If not specified then the allocated blocks of the VolumeSnapshot
	// identified by SnapshotName will be enumerated.
	PrevSnapshotName string

	// StartingOffset is the initial byte offset.
	StartingOffset int64

	// MaxResults is the number of tuples to return in each record.
	// If 0 then the CSI driver decides the value.
	MaxResults int32

	// CSIDriver specifies the name of the CSI driver and is used to
	// load the associated SnapshotMetadataService CR.
	// The field is optional. If not specified then it
	// will be fetched from the VolumeSnapshotContent of
	// the VolumeSnapshot specified by the SnapshotName field.
	CSIDriver string

	// Specify the ServiceAccount object used to construct a security token
	// with the audience string from the SnapshotMetadataService CR.
	// If either of the following fields are unspecified, the default for the given client will be used.
	SANamespace string
	SAName      string

	// TokenExpirySecs specifies the time in seconds after which the
	// security token will expire.
	// If unspecified then the value of DefaultTokenExpirySeconds is used.
	TokenExpirySecs int64
}

func (a Args) Validate() error {
	switch {
	case a.Emitter == nil:
		return fmt.Errorf("%w: missing Emitter", ErrInvalidArgs)
	case a.Namespace == "":
		return fmt.Errorf("%w: missing Namespace", ErrInvalidArgs)
	case a.SnapshotName == "":
		return fmt.Errorf("%w: missing SnapshotName", ErrInvalidArgs)
	case a.TokenExpirySecs < 0:
		return fmt.Errorf("%w: invalid TokenExpirySecs", ErrInvalidArgs)
	case a.MaxResults < 0:
		return fmt.Errorf("%w: invalid MaxResults", ErrInvalidArgs)
	case a.SANamespace == "" && a.SAName != "":
		return fmt.Errorf("%w: SAName provided but SANamespace missing", ErrInvalidArgs)
	case a.SANamespace != "" && a.SAName == "":
		return fmt.Errorf("%w: SANamespace provided but SAName missing", ErrInvalidArgs)
	}

	if err := a.Clients.Validate(); err != nil {
		return err
	}

	return nil
}

// IteratorMetadata returns a single metadata record.
// These fields are fetched from the stream returned by either
// GetMetadataAllocated or GetMetadataDelta.
type IteratorMetadata struct {
	BlockMetadataType   api.BlockMetadataType `json:"block_metadata_type"`
	VolumeCapacityBytes int64                 `json:"volume_capacity_bytes"`
	BlockMetadata       []*api.BlockMetadata  `json:"block_metadata"`
}

type IteratorEmitter interface {
	// SnapshotMetadataIteratorRecord is invoked for each record received
	// from the gRPC stream.
	SnapshotMetadataIteratorRecord(recordNumber int, metadata IteratorMetadata) error

	// SnapshotMetadataIteratorDone is called prior to termination as long as
	// no error was encountered.
	SnapshotMetadataIteratorDone(numberRecords int) error
}

type Iterator struct {
	Args
	recordNum int

	h iteratorHelpers
}

type iteratorHelpers interface {
	getCSIDriverFromPrimarySnapshot(ctx context.Context) (string, error)
	getDefaultServiceAccount(ctx context.Context) (saNamespace string, saName string, err error)
	getSnapshotMetadataServiceCR(ctx context.Context, csiDriver string) (*smsCRv1alpha1.SnapshotMetadataService, error)
	createSecurityToken(ctx context.Context, saNamespace, saName, audience string) (string, error)
	getGRPCClient(caCert []byte, URL string) (api.SnapshotMetadataClient, error)
	getAllocatedBlocks(ctx context.Context, grpcClient api.SnapshotMetadataClient, securityToken string) error
	getChangedBlocks(ctx context.Context, grpcClient api.SnapshotMetadataClient, securityToken string) error
}

func NewIterator(args Args) *Iterator {
	iter := &Iterator{}
	iter.Args = args
	iter.h = iter

	if iter.TokenExpirySecs == 0 {
		iter.TokenExpirySecs = DefaultTokenExpirySeconds
	}

	return iter
}

// run will invoke the emitter's SnapshotMetadataIteratorRecord
// operation for each record received from the CSI driver.
// If the enumeration is aborted by the operation then it will
// return ErrCancelled.
// When the enumeration terminates normally the emitter's
// SnapshotMetadataIteratorDone operation is invoked.
func (iter *Iterator) Run(ctx context.Context) error {
	var err error

	saName := iter.SAName           // optional field
	saNamespace := iter.SANamespace // optional field
	if saName == "" {
		saNamespace, saName, err = iter.h.getDefaultServiceAccount(ctx)
		if err != nil {
			return err
		}
	}

	csiDriver := iter.CSIDriver // optional field
	if csiDriver == "" {
		if csiDriver, err = iter.h.getCSIDriverFromPrimarySnapshot(ctx); err != nil {
			return err
		}
	}

	// load the driver's SnapshotMetadataService object
	smsCR, err := iter.h.getSnapshotMetadataServiceCR(ctx, csiDriver)
	if err != nil {
		return err
	}

	// get the security token to use in the API
	securityToken, err := iter.h.createSecurityToken(ctx, saNamespace, saName, smsCR.Spec.Audience)
	if err != nil {
		return err
	}

	// create the snapshot metadata service gRPC client
	apiClient, err := iter.h.getGRPCClient(smsCR.Spec.CACert, smsCR.Spec.Address)
	if err != nil {
		return err
	}

	// Create a cancellable child context to terminate the server's
	// metadata stream in case the emitter aborts.
	ctx, cancelFn := context.WithCancel(ctx)
	defer cancelFn()

	if iter.PrevSnapshotName == "" {
		err = iter.h.getAllocatedBlocks(ctx, apiClient, securityToken)
	} else {
		err = iter.h.getChangedBlocks(ctx, apiClient, securityToken)
	}
	if err != nil {
		return err
	}

	return iter.Emitter.SnapshotMetadataIteratorDone(iter.recordNum)
}

func (iter *Iterator) getDefaultServiceAccount(ctx context.Context) (namespace string, name string, err error) {
	ssr, err := iter.KubeClient.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authv1.SelfSubjectReview{}, apimetav1.CreateOptions{})
	if err != nil {
		return "", "", fmt.Errorf("SelfSubjectReviews.Create(): %w", err)
	}
	if strings.HasPrefix(ssr.Status.UserInfo.Username, K8sServiceAccountUserNamePrefix) {
		fields := strings.Split(ssr.Status.UserInfo.Username, ":")
		if len(fields) == 4 {
			return fields[2], fields[3], nil
		}
	}

	return "", "", fmt.Errorf("%w: ServiceAccount unspecified and default cannot be determined", ErrInvalidArgs)
}

// getCSIDriverFromPrimarySnapshot loads the bound VolumeSnapshotContent
// of the VolumeSnapshot identified by SnapshotName to fetch the CSI driver.
func (iter *Iterator) getCSIDriverFromPrimarySnapshot(ctx context.Context) (string, error) {
	vs, err := iter.SnapshotClient.SnapshotV1().VolumeSnapshots(iter.Namespace).Get(ctx, iter.SnapshotName, apimetav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("VolumeSnapshots.Get(%s/%s): %w", iter.Namespace, iter.SnapshotName, err)
	}

	if vs.Status == nil || vs.Status.BoundVolumeSnapshotContentName == nil {
		return "", fmt.Errorf("VolumeSnapshot(%s/%s) has no bound VolumeSnapshotContent", vs.Namespace, vs.Name)
	}

	vsc, err := iter.SnapshotClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, *vs.Status.BoundVolumeSnapshotContentName, apimetav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("VolumeSnapshotContents.Get(%s) for VolumeSnapshot(%s/%s): %w",
			*vs.Status.BoundVolumeSnapshotContentName,
			vs.Namespace, vs.Name, err)
	}

	return vsc.Spec.Driver, nil
}

func (iter *Iterator) getSnapshotMetadataServiceCR(ctx context.Context, csiDriver string) (*smsCRv1alpha1.SnapshotMetadataService, error) {
	sms, err := iter.SmsCRClient.CbtV1alpha1().SnapshotMetadataServices().Get(ctx, csiDriver, apimetav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("SnapshotMetadataServices.Get(%s): %w", csiDriver, err)
	}

	return sms, nil
}

// createSecurityToken will create a security token for the specified storage
// account using the audience string from the SnapshotMetadataService CR.
func (iter *Iterator) createSecurityToken(ctx context.Context, saNamespace, sa, audience string) (string, error) {
	tokenRequest := authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &iter.TokenExpirySecs,
		},
	}

	tokenResp, err := iter.KubeClient.CoreV1().ServiceAccounts(saNamespace).
		CreateToken(ctx, sa, &tokenRequest, apimetav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("ServiceAccounts.CreateToken(%s/%s): %v", saNamespace, sa, err)
	}

	return tokenResp.Status.Token, nil
}

func (iter *Iterator) getGRPCClient(caCert []byte, url string) (api.SnapshotMetadataClient, error) {
	// Add the CA to the cert pool
	certPool := x509.NewCertPool()
	if !certPool.AppendCertsFromPEM(caCert) {
		return nil, ErrCACert
	}

	tlsCredentials := grpcCreds.NewTLS(&tls.Config{RootCAs: certPool})
	conn, err := grpc.NewClient(url, grpc.WithTransportCredentials(tlsCredentials))
	if err != nil {
		return nil, fmt.Errorf("grpc.NewClient(%s): %w", url, err)
	}

	return api.NewSnapshotMetadataClient(conn), nil
}

func (iter *Iterator) getAllocatedBlocks(ctx context.Context, grpcClient api.SnapshotMetadataClient, securityToken string) error {
	stream, err := grpcClient.GetMetadataAllocated(ctx, &api.GetMetadataAllocatedRequest{
		SecurityToken:  securityToken,
		Namespace:      iter.Namespace,
		SnapshotName:   iter.SnapshotName,
		StartingOffset: iter.StartingOffset,
		MaxResults:     iter.MaxResults,
	})
	if err != nil {
		return fmt.Errorf("GetMetadataAllocated(%s,%s): %w", iter.Namespace, iter.SnapshotName, err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("GetMetadataAllocated(%s,%s).Recv: %w", iter.Namespace, iter.SnapshotName, err)
		}

		iter.recordNum++

		err = iter.Emitter.SnapshotMetadataIteratorRecord(iter.recordNum, IteratorMetadata{
			BlockMetadataType:   resp.BlockMetadataType,
			VolumeCapacityBytes: resp.VolumeCapacityBytes,
			BlockMetadata:       resp.BlockMetadata,
		})
		if err != nil {
			return err
		}
	}
}

func (iter *Iterator) getChangedBlocks(ctx context.Context, grpcClient api.SnapshotMetadataClient, securityToken string) error {
	stream, err := grpcClient.GetMetadataDelta(ctx, &api.GetMetadataDeltaRequest{
		SecurityToken:      securityToken,
		Namespace:          iter.Namespace,
		BaseSnapshotName:   iter.PrevSnapshotName,
		TargetSnapshotName: iter.SnapshotName,
		StartingOffset:     iter.StartingOffset,
		MaxResults:         iter.MaxResults,
	})
	if err != nil {
		return fmt.Errorf("GetMetadataDelta(%s,%s,%s): %w", iter.Namespace, iter.PrevSnapshotName, iter.SnapshotName, err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("GetMetadataDelta(%s,%s,%s).Recv: %w", iter.Namespace, iter.PrevSnapshotName, iter.SnapshotName, err)
		}

		iter.recordNum++

		err = iter.Emitter.SnapshotMetadataIteratorRecord(iter.recordNum, IteratorMetadata{
			BlockMetadataType:   resp.BlockMetadataType,
			VolumeCapacityBytes: resp.VolumeCapacityBytes,
			BlockMetadata:       resp.BlockMetadata,
		})
		if err != nil {
			return err
		}
	}
}

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

package grpc

import (
	"context"
	"io"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/kubernetes-csi/external-snapshot-metadata/pkg/api"
)

func (s *Server) GetMetadataAllocated(req *api.GetMetadataAllocatedRequest, stream api.SnapshotMetadata_GetMetadataAllocatedServer) error {
	ctx := s.getMetadataAllocatedContextWithLogger(req, stream)

	if err := s.validateGetMetadataAllocatedRequest(req); err != nil {
		klog.FromContext(ctx).Error(err, "validation failed")
		return err
	}

	if err := s.authenticateAndAuthorize(ctx, req.SecurityToken, req.Namespace); err != nil {
		return err
	}

	if err := s.isCSIDriverReady(ctx); err != nil {
		return err
	}

	csiReq, err := s.convertToCSIGetMetadataAllocatedRequest(ctx, req)
	if err != nil {
		return err
	}

	// Invoke the CSI Driver's GetMetadataDelta gRPC and stream the response back to client
	klog.FromContext(ctx).V(HandlerTraceLogLevel).Info("calling CSI driver", "snapshotId", csiReq.SnapshotId)
	csiStream, err := csi.NewSnapshotMetadataClient(s.csiConnection()).GetMetadataAllocated(ctx, csiReq)
	if err != nil {
		return err
	}

	return s.streamGetMetadataAllocatedResponse(ctx, stream, csiStream)
}

// getMetadataAllocatedContextWithLogger returns the stream context with an embedded
// contextual logger primed with a description of the request.
func (s *Server) getMetadataAllocatedContextWithLogger(req *api.GetMetadataAllocatedRequest, stream api.SnapshotMetadata_GetMetadataAllocatedServer) context.Context {
	return klog.NewContext(stream.Context(),
		klog.LoggerWithValues(klog.Background(),
			"op", s.OperationID("GetMetadataAllocated"),
			"namespace", req.Namespace,
			"snapshotName", req.SnapshotName,
			"startingOffset", req.StartingOffset,
			"maxResults", req.MaxResults,
		))
}

func (s *Server) validateGetMetadataAllocatedRequest(req *api.GetMetadataAllocatedRequest) error {
	if len(req.GetSecurityToken()) == 0 {
		return status.Errorf(codes.InvalidArgument, msgInvalidArgumentSecurityTokenMissing)
	}

	if len(req.GetNamespace()) == 0 {
		return status.Errorf(codes.InvalidArgument, msgInvalidArgumentNamespaceMissing)
	}

	if len(req.GetSnapshotName()) == 0 {
		return status.Errorf(codes.InvalidArgument, msgInvalidArgumentSnaphotNameMissing)
	}

	return nil
}

func (s *Server) convertToCSIGetMetadataAllocatedRequest(ctx context.Context, req *api.GetMetadataAllocatedRequest) (*csi.GetMetadataAllocatedRequest, error) {
	vsi, err := s.getVolSnapshotInfo(ctx, req.Namespace, req.SnapshotName)
	if err != nil {
		return nil, err
	}

	if vsi.DriverName != s.driverName() {
		err = status.Errorf(codes.InvalidArgument, msgInvalidArgumentSnaphotDriverInvalidFmt, req.SnapshotName, s.driverName())
		klog.FromContext(ctx).Error(err, "invalid driver")
		return nil, err
	}

	secretsMap, err := s.getSnapshotterCredentials(ctx, vsi)
	if err != nil {
		return nil, err
	}

	return &csi.GetMetadataAllocatedRequest{
		SnapshotId:     vsi.SnapshotHandle,
		StartingOffset: req.StartingOffset,
		MaxResults:     req.MaxResults,
		Secrets:        secretsMap,
	}, nil
}

func (s *Server) streamGetMetadataAllocatedResponse(ctx context.Context, clientStream api.SnapshotMetadata_GetMetadataAllocatedServer, csiStream csi.SnapshotMetadata_GetMetadataAllocatedClient) error {
	for {
		csiResp, err := csiStream.Recv()
		if err == io.EOF {
			klog.FromContext(ctx).V(HandlerTraceLogLevel).Info("stream EOF")
			return nil
		}

		//TODO: stream logging with progress

		if err != nil {
			return s.statusPassOrWrapError(err, codes.Internal, msgInternalFailedCSIDriverResponseFmt, err)
		}

		clientResp := s.convertToGetMetadataAllocatedResponse(csiResp)
		if err := clientStream.Send(clientResp); err != nil {
			return s.statusPassOrWrapError(err, codes.Internal, msgInternalFailedtoSendResponseFmt, err)
		}
	}
}

func (s *Server) convertToGetMetadataAllocatedResponse(csiResp *csi.GetMetadataAllocatedResponse) *api.GetMetadataAllocatedResponse {
	apiResp := &api.GetMetadataAllocatedResponse{
		BlockMetadataType:   api.BlockMetadataType(csiResp.BlockMetadataType),
		VolumeCapacityBytes: csiResp.VolumeCapacityBytes,
	}

	for _, b := range csiResp.GetBlockMetadata() {
		apiResp.BlockMetadata = append(apiResp.BlockMetadata, &api.BlockMetadata{
			ByteOffset: b.ByteOffset,
			SizeBytes:  b.SizeBytes,
		})
	}

	return apiResp
}
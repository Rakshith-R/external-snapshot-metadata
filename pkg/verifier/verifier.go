/*
Copyright 2025 The Kubernetes Authors.

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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	iter "github.com/kubernetes-csi/external-snapshot-metadata/pkg/iterator"
)

// VerifySnapshotMetadata enumerates either the allocated blocks of a
// VolumeSnapshot object, or the blocks changed between a pair of
// VolumeSnapshot objects.
//
// Metadata is returned via an emitter interface specified in the
// invocation arguments. Iteration terminates on the first error
// encountered, or if requested by the emitter.
func VerifySnapshotMetadata(ctx context.Context, args Args) error {
	if err := args.Validate(); err != nil {
		return err
	}

	return newVerifierIterator(args).Run(ctx)
}

type Args struct {
	iter.Args

	// SourceDevicePath is optional, and if specified SourceDevice will be used to copy
	// changed blocks to the TargetDevice.
	SourceDevicePath string

	// TargetDevice is optional, and if specified changed blocks from the SourceDevice
	// will be copied to it.
	TargetDevicePath string
}

func (a *Args) Validate() error {
	err := a.Args.Validate()
	if err != nil {
		return err
	}

	if a.SourceDevicePath == "" || a.TargetDevicePath == "" {
		return fmt.Errorf("%w: Verify requires SourceDevicePath and TargetDevicePath", iter.ErrInvalidArgs)
	}

	if err = a.Clients.Validate(); err != nil {
		return err
	}

	return nil
}

type VerifierIterator struct {
	iter.Iterator
	Args
}

func newVerifierIterator(args Args) *VerifierIterator {
	verifierIter := &VerifierIterator{
		Iterator: iter.New(args.Args),
		Args:     args,
	}

	return verifierIter
}

// VerifierEmitter formats the metadata as a table.
type VerifierEmitter struct {
	// SourceDevice contains the source device file descriptor.
	SourceDevice *os.File

	// TargetDevice contains the target device file descriptor.
	TargetDevice *os.File
}

func (verifierEmitter *VerifierEmitter) SnapshotMetadataIteratorRecord(_ int, metadata iter.IteratorMetadata) error {
	for _, bmd := range metadata.BlockMetadata {
		buffer := make([]byte, bmd.SizeBytes)
		// Seek to the block's offset in the source device.
		_, err := verifierEmitter.SourceDevice.Seek(bmd.ByteOffset, io.SeekStart)
		if err != nil {
			return fmt.Errorf("failed to seek source device(offset: %d, size bytes: %d): %w", bmd.ByteOffset, bmd.SizeBytes, err)
		}

		// Read the block from the source device.
		_, err = verifierEmitter.SourceDevice.Read(buffer)
		if err != nil {
			return fmt.Errorf("failed to read source device(offset: %d, size bytes: %d): %w", bmd.ByteOffset, bmd.SizeBytes, err)
		}

		// Write the block to the target device at designated offset.
		_, err = verifierEmitter.TargetDevice.WriteAt(buffer, bmd.ByteOffset)
		if err != nil {
			return fmt.Errorf("failed to write target device(offset: %d, size bytes: %d): %w", bmd.ByteOffset, bmd.SizeBytes, err)
		}
	}

	return nil
}

// SnapshotMetadataIteratorDone will compare the contents of the source and target devices.
func (verifierEmitter *VerifierEmitter) SnapshotMetadataIteratorDone(_ int) error {
	// Seek to the start of the source and target devices.
	_, err := verifierEmitter.SourceDevice.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek source device(%q) to start: %w", verifierEmitter.SourceDevice.Name(), err)
	}
	_, err = verifierEmitter.TargetDevice.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to seek target device(%q) to start: %w", verifierEmitter.TargetDevice.Name(), err)
	}

	const chunkSize = 256
	sourceBuffer := make([]byte, chunkSize)
	targetBuffer := make([]byte, chunkSize)
	for {
		// Read a chunk from the source and target devices.
		_, sourceErr := verifierEmitter.SourceDevice.Read(sourceBuffer)
		_, targetErr := verifierEmitter.TargetDevice.Read(targetBuffer)

		if sourceErr != nil || targetErr != nil {
			if sourceErr == io.EOF && targetErr == io.EOF {
				// Both devices have been read completely.
				return nil
			} else if sourceErr == io.EOF || targetErr == io.EOF {
				// One device has been read completely but the other has not.
				return fmt.Errorf("source and target device contents do not match")
			} else {
				// An error occurred while reading from both devices.
				return fmt.Errorf("error reading source and target device contents: source(%q) target(%q)", sourceErr, targetErr)
			}
		}

		if !bytes.Equal(sourceBuffer, targetBuffer) {
			return fmt.Errorf("source and target device contents do not match")
		}
	}
}

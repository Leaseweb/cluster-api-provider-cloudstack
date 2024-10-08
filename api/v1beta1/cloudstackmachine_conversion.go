/*
Copyright 2022 The Kubernetes Authors.

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

package v1beta1

import (
	machineryconversion "k8s.io/apimachinery/pkg/conversion"
	utilconversion "sigs.k8s.io/cluster-api/util/conversion"
	"sigs.k8s.io/controller-runtime/pkg/conversion"

	"sigs.k8s.io/cluster-api-provider-cloudstack/api/v1beta3"
)

func (r *CloudStackMachine) ConvertTo(dstRaw conversion.Hub) error {
	dst := dstRaw.(*v1beta3.CloudStackMachine)

	if err := Convert_v1beta1_CloudStackMachine_To_v1beta3_CloudStackMachine(r, dst, nil); err != nil {
		return err
	}

	// Manually restore data
	restored := &v1beta3.CloudStackMachine{}
	if ok, err := utilconversion.UnmarshalData(r, restored); err != nil || !ok {
		return err
	}
	dst.Spec.FailureDomainName = restored.Spec.FailureDomainName
	dst.Spec.UncompressedUserData = restored.Spec.UncompressedUserData

	// Don't bother converting empty disk offering objects
	if restored.Spec.DiskOffering.MountPath != "" {
		dst.Spec.DiskOffering = &v1beta3.CloudStackResourceDiskOffering{
			CustomSize: restored.Spec.DiskOffering.CustomSize,
			MountPath:  restored.Spec.DiskOffering.MountPath,
			Device:     restored.Spec.DiskOffering.Device,
			Filesystem: restored.Spec.DiskOffering.Filesystem,
			Label:      restored.Spec.DiskOffering.Label,
		}
		dst.Spec.DiskOffering.ID = restored.Spec.DiskOffering.ID
		dst.Spec.DiskOffering.Name = restored.Spec.DiskOffering.Name
	}

	if restored.Status.Status != nil {
		dst.Status.Status = restored.Status.Status
	}
	if restored.Status.Reason != nil {
		dst.Status.Reason = restored.Status.Reason
	}

	return nil
}

func (r *CloudStackMachine) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta3.CloudStackMachine)

	if err := Convert_v1beta3_CloudStackMachine_To_v1beta1_CloudStackMachine(src, r, nil); err != nil {
		return err
	}

	// Preserve Hub data on down-conversion
	return utilconversion.MarshalData(src, r)
}

func Convert_v1beta1_CloudStackMachineSpec_To_v1beta3_CloudStackMachineSpec(in *CloudStackMachineSpec, out *v1beta3.CloudStackMachineSpec, scope machineryconversion.Scope) error {
	return autoConvert_v1beta1_CloudStackMachineSpec_To_v1beta3_CloudStackMachineSpec(in, out, scope)
}

func Convert_v1beta3_CloudStackMachineSpec_To_v1beta1_CloudStackMachineSpec(in *v1beta3.CloudStackMachineSpec, out *CloudStackMachineSpec, s machineryconversion.Scope) error {
	return autoConvert_v1beta3_CloudStackMachineSpec_To_v1beta1_CloudStackMachineSpec(in, out, s)
}

func Convert_v1beta3_CloudStackMachineStatus_To_v1beta1_CloudStackMachineStatus(in *v1beta3.CloudStackMachineStatus, out *CloudStackMachineStatus, s machineryconversion.Scope) error {
	return autoConvert_v1beta3_CloudStackMachineStatus_To_v1beta1_CloudStackMachineStatus(in, out, s)
}

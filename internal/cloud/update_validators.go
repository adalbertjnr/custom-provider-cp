package cloud

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/provider-customcomputeprovider/apis/compute/v1alpha1"
	"github.com/crossplane/provider-customcomputeprovider/internal/generic"
)

func NeedsAMIUpdate(current *types.Instance, desired *v1alpha1.InstanceConfig) bool {
	return *current.ImageId != desired.InstanceAMI
}

func NeedsInstanceTypeUpdate(current *types.Instance, desired *v1alpha1.InstanceConfig) bool {
	return current.InstanceType != types.InstanceType(desired.InstanceType)
}

func NeedsInstanceNameUpdate(current *types.Instance, desired *v1alpha1.InstanceConfig) bool {
	currentInstanceTags := generic.FromSliceToMapWithValues(current.Tags,
		func(tag types.Tag) (string, string) { return *tag.Key, *tag.Value },
	)

	if currentName, found := currentInstanceTags[INSTANCE_TAG_KEY_NAME]; found {
		return desired.InstanceName != currentName
	}

	return false
}

func NeedsTagsUpdate(current *types.Instance, desired *v1alpha1.InstanceConfig) bool {
	currentTags := generic.FromSliceToMapWithValues(current.Tags,
		func(tag types.Tag) (string, string) {
			return *tag.Key, *tag.Value
		})

	for dk, dv := range desired.InstanceTags {
		if cv, found := currentTags[dk]; !found {
			return true
		} else {
			if cv != dv {
				return true
			}
		}
	}

	for ck, cv := range currentTags {
		if dv, found := desired.InstanceTags[ck]; !found {
			if ck == "Name" {
				continue
			}
			return true
		} else {
			if cv != dv {
				return true
			}
		}
	}

	return false
}

func NeedsSecurityGroupsUpdate(current *types.Instance, desired *v1alpha1.InstanceConfig) bool {
	currentMapSGIds := generic.FromSliceToMap(current.SecurityGroups,
		func(security types.GroupIdentifier) string { return *security.GroupId },
	)

	desiredMapSGIds := generic.FromSliceToMap(desired.Networking.InstanceSecurityGroups,
		func(securityGroupId string) string { return securityGroupId },
	)

	currentSGIds := current.SecurityGroups
	desiredSGIds := desired.Networking.InstanceSecurityGroups

	for _, dsg := range desiredSGIds {
		if _, exists := currentMapSGIds[dsg]; !exists {
			return true
		}
	}

	for _, csg := range currentSGIds {
		if _, exists := desiredMapSGIds[*csg.GroupId]; !exists {
			return true
		}
	}

	return false
}

func NeedsVolumeUpdate(ctx context.Context, c *EC2Client, current *types.Instance, desired *v1alpha1.InstanceConfig) bool {
	output, err := c.Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []string{*current.InstanceId}},
		},
	})

	if err != nil {
		return false
	}

	commands := VolumeValidator(output, current, desired)
	return len(commands) > 0
}

func ResourceUpToDate(ctx context.Context, c *EC2Client, l logging.Logger, current *types.Instance, desired *v1alpha1.InstanceConfig) bool {
	amiExp := NeedsAMIUpdate(current, desired)
	typExp := NeedsInstanceTypeUpdate(current, desired)
	tagExp := NeedsTagsUpdate(current, desired)
	secExp := NeedsSecurityGroupsUpdate(current, desired)
	volExp := NeedsVolumeUpdate(ctx, c, current, desired)
	namExp := NeedsInstanceNameUpdate(current, desired)

	l.Info("observe check",
		"needs name update", namExp,
		"needs ami update", amiExp,
		"needs type update", typExp,
		"needs tag update", tagExp,
		"needs security groups update", secExp,
		"needs volume update", volExp,
	)

	return !amiExp && !typExp && !tagExp && !secExp && !volExp && !namExp
}

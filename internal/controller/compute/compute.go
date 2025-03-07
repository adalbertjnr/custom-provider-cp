/*
Copyright 2022 The Crossplane Authors.

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

package compute

import (
	"context"
	"encoding/json"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/pkg/connection"
	"github.com/crossplane/crossplane-runtime/pkg/controller"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/pkg/resource"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/crossplane/provider-customcomputeprovider/apis/compute/v1alpha1"
	apisv1alpha1 "github.com/crossplane/provider-customcomputeprovider/apis/v1alpha1"
	"github.com/crossplane/provider-customcomputeprovider/internal/cloud"
	"github.com/crossplane/provider-customcomputeprovider/internal/features"
)

const (
	errNotCompute   = "managed resource is not a Compute custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCreds     = "cannot get credentials"

	errNewClient = "cannot create new Service"
	errAwsClient = "cannot create aws client"
)

// A NoOpService does nothing.
type NoOpService struct{}

var (
	newNoOpService = func(_ []byte) (interface{}, error) { return &NoOpService{}, nil }
)

// Setup adds a controller that reconciles Compute managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.ComputeGroupKind)

	cps := []managed.ConnectionPublisher{managed.NewAPISecretPublisher(mgr.GetClient(), mgr.GetScheme())}
	if o.Features.Enabled(features.EnableAlphaExternalSecretStores) {
		cps = append(cps, connection.NewDetailsManager(mgr.GetClient(), apisv1alpha1.StoreConfigGroupVersionKind))
	}

	r := managed.NewReconciler(mgr,
		resource.ManagedKind(v1alpha1.ComputeGroupVersionKind),
		managed.WithExternalConnecter(&connector{
			kube:         mgr.GetClient(),
			usage:        resource.NewProviderConfigUsageTracker(mgr.GetClient(), &apisv1alpha1.ProviderConfigUsage{}),
			newServiceFn: newNoOpService,
			logger:       o.Logger,
		},
		),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
		managed.WithConnectionPublishers(cps...))

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Compute{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

// A connector is expected to produce an ExternalClient when its Connect method
// is called.
type connector struct {
	kube         client.Client
	usage        resource.Tracker
	newServiceFn func(creds []byte) (interface{}, error)
	logger       logging.Logger
}

// Connect typically produces an ExternalClient by:
// 1. Tracking that the managed resource is using a ProviderConfig.
// 2. Getting the managed resource's ProviderConfig.
// 3. Getting the credentials specified by the ProviderConfig.
// 4. Using the credentials to form a client.
func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Compute)
	if !ok {
		return nil, errors.New(errNotCompute)
	}

	if err := c.usage.Track(ctx, mg); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	credentialsClient := func(creds []byte) (interface{}, error) {
		var awsCredentials struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
		}

		if err := json.Unmarshal(creds, &awsCredentials); err != nil {
			return nil, err
		}

		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithRegion(cr.Spec.ForProvider.AWSConfig.Region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
				awsCredentials.AccessKeyID,
				awsCredentials.SecretAccessKey,
				"",
			)),
		)

		if err != nil {
			return nil, err
		}

		client := ec2.NewFromConfig(cfg)
		return &cloud.EC2Client{Client: client}, nil

	}

	pc := &apisv1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: cr.GetProviderConfigReference().Name}, pc); err != nil {
		return &external{}, nil
	}

	cd := pc.Spec.Credentials
	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	svc, err := credentialsClient(data)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{service: svc, logger: c.logger}, nil
}

// An ExternalClient observes, then either creates, updates, or deletes an
// external resource to ensure it reflects the managed resource's desired state.
type external struct {
	// A 'client' used to connect to the external resource API. In practice this
	// would be something like an AWS SDK client.
	service interface{}
	logger  logging.Logger
}

func (c *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Compute)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotCompute)
	}

	client, err := clientSelector(ctx, c, cr.Spec.ForProvider.AWSConfig.Region)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	resourceConfig := cr.Spec.ForProvider.InstanceConfig

	found, currentResource, err := client.Observe(ctx, resourceConfig.InstanceName)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	c.logger.Info("observe", "status", found)

	if !found {
		c.logger.Info("observe", "status", "resource not found")
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	if !cloud.EC2ResourceUpToDate(currentResource, &resourceConfig) {
		c.logger.Info("observe", "status", "resource exists but must to be updated")

		return managed.ExternalObservation{
			ResourceExists:   true,
			ResourceUpToDate: false,
		}, nil
	}

	c.logger.Info("observe", "status", "resource exists and do not need to be updated")
	return managed.ExternalObservation{
		// Return false when the external resource does not exist. This lets
		// the managed resource reconciler know that it needs to call Create to
		// (re)create the resource, or that it has successfully been deleted.
		ResourceExists: true,

		// Return false when the external resource exists, but it not up to date
		// with the desired managed resource state. This lets the managed
		// resource reconciler know that it needs to call Update.
		ResourceUpToDate: true,

		// Return any details that may be required to connect to the external
		// resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Compute)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotCompute)
	}

	client, err := clientSelector(ctx, c, cr.Spec.ForProvider.AWSConfig.Region)
	if err != nil {
		return managed.ExternalCreation{}, err
	}

	c.logger.Info("create", "status", "resource will be created")
	_, err = client.CreateInstance(ctx, cr.Spec.ForProvider.InstanceConfig)
	if err != nil {
		return managed.ExternalCreation{}, err
	}

	c.logger.Info("create", "status", "resource is created successfully")
	return managed.ExternalCreation{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	cr, ok := mg.(*v1alpha1.Compute)
	if !ok {
		return managed.ExternalUpdate{}, errors.New(errNotCompute)
	}

	client, err := clientSelector(ctx, c, cr.Spec.ForProvider.AWSConfig.Region)

	if err != nil {
		return managed.ExternalUpdate{}, err
	}

	c.logger.Info("update", "status", "retrieving resource to be compared against the desired state")
	current, err := client.GetInstance(ctx, cr.Spec.ForProvider.InstanceConfig.InstanceName)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}

	updates := []func(*ec2types.Instance, *v1alpha1.InstanceConfig) error{}

	if cloud.InstanceAMIUpdate(
		current,
		&cr.Spec.ForProvider.InstanceConfig,
	) {
		c.logger.Info("update", "status", "ami difference", "action", "update resource ami")
		updates = append(updates, client.EC2HandleInstanceAMI)
	}

	if cloud.InstanceTypeUpdate(
		current,
		&cr.Spec.ForProvider.InstanceConfig,
	) {
		c.logger.Info("update", "status", "instance type difference", "action", "update resource type")
		updates = append(updates, client.EC2HandleInstanceType)
	}

	if cloud.InstanceTagsUpdate(
		current,
		&cr.Spec.ForProvider.InstanceConfig,
	) {
		c.logger.Info("update", "status", "instance tags difference", "action", "update resource tags")
		updates = append(updates, client.EC2HandleInstanceTags)
	}

	if cloud.InstanceSecurityGroupsUpdate(
		current,
		&cr.Spec.ForProvider.InstanceConfig,
	) {
		c.logger.Info("update", "status", "instance security groups difference", "action", "update security groups tags")
		updates = append(updates, client.EC2HandleInstanceSecurityGroups)
	}

	for _, update := range updates {
		if err := update(current, &cr.Spec.ForProvider.InstanceConfig); err != nil {
			return managed.ExternalUpdate{}, err
		}
	}

	return managed.ExternalUpdate{
		// Optionally return any details that may be required to connect to the
		// external resource. These will be stored as the connection secret.
		ConnectionDetails: managed.ConnectionDetails{},
	}, nil
}

func (c *external) Delete(ctx context.Context, mg resource.Managed) error {
	cr, ok := mg.(*v1alpha1.Compute)
	if !ok {
		return errors.New(errNotCompute)
	}

	client, err := clientSelector(ctx, c, cr.Spec.ForProvider.AWSConfig.Region)
	if err != nil {
		return err
	}

	c.logger.Info("delete", "status", "resource is being deleted")
	return client.DeleteInstance(ctx, cr.Spec.ForProvider.InstanceConfig)
}

func clientSelector(ctx context.Context, c *external, region string) (*cloud.EC2Client, error) {
	var client *cloud.EC2Client

	cc, ok := c.service.(*cloud.EC2Client)
	if ok {
		c.logger.Info("client selector", "client selected", "external")
		client = cc
	} else {
		cfg, err := cloud.AWSClientConnector(ctx)(region)
		if err != nil {
			return nil, errors.New(errAwsClient)
		}

		c.logger.Info("client selector", "client selected", "internal")
		cc := cloud.NewEC2Client(cfg)
		client = cc
	}

	return client, nil
}

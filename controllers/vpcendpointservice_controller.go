/*


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

package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/route53"
	backoff "github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Defaults for exponential backoff and Istio service annotation keys
const (
	DefaultInitialInterval         = 500 * time.Millisecond
	DefaultRandomizationFactor     = 0.5
	DefaultMultiplier              = 1.5
	DefaultMaxInterval             = 15 * time.Second
	DefaultMaxElapsedTime          = 320 * time.Second
	endpointAnnotationVpcID        = "vpc-endpoint-service-operator.fil.com/endpoint-vpc-id"
	endpointAnnotationHostname     = "vpc-endpoint-service-operator.fil.com/hostname"
	endpointAnnotationHostedZoneID = "vpc-endpoint-service-operator.fil.com/hosted-zone-id"
)

// VpcEndpointServiceReconciler reconciles a VpcEndpointService object
type VpcEndpointServiceReconciler struct {
	ElbSvc     *elbv2.ELBV2
	Ec2Svc     *ec2.EC2
	Route53Svc *route53.Route53
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// NewVpcEndpointServiceReconciler creates default reconsiler
func NewVpcEndpointServiceReconciler(mgr manager.Manager, log logr.Logger) (*VpcEndpointServiceReconciler, error) {
	session := session.Must(session.NewSession())
	ec2Svc := ec2.New(session)
	route53Svc := route53.New(session)

	return &VpcEndpointServiceReconciler{
		ElbSvc:     elbv2.New(session),
		Ec2Svc:     ec2Svc,
		Route53Svc: route53Svc,
		Client:     mgr.GetClient(),
		Log:        log,
		Scheme:     mgr.GetScheme(),
	}, nil
}

// +kubebuilder:rbac:groups=vpc-endpoint-service.fil.com,resources=vpcendpointservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vpc-endpoint-service.fil.com,resources=vpcendpointservices/status,verbs=get;update;patch

func ignoreNonLoadBalancerServicePredicate() predicate.Predicate {
	return predicate.Funcs{
		GenericFunc: func(e event.GenericEvent) bool {
			o := e.Object.(*v1.Service)
			if o.Spec.Type != "LoadBalancer" {
				return false
			}
			return true
		},
		CreateFunc: func(e event.CreateEvent) bool {
			obj, ok := e.Object.(*v1.Service)
			if !ok {
				return false
			}
			if obj.Spec.Type != "LoadBalancer" {
				return false
			}
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return false
			}
			obj, ok := e.ObjectNew.(*v1.Service)
			if !ok {
				return false
			}
			if obj.Spec.Type != "LoadBalancer" {
				return false
			}
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			obj, ok := e.Object.(*v1.Service)
			if !ok {
				return false
			}
			if obj.Spec.Type != "LoadBalancer" {
				return false
			}
			return true
		},
	}
}

func (r *VpcEndpointServiceReconciler) waitForLoadBalancer(service v1.Service) (err error) {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     DefaultInitialInterval,
		RandomizationFactor: DefaultRandomizationFactor,
		Multiplier:          DefaultMultiplier,
		MaxInterval:         DefaultMaxInterval,
		MaxElapsedTime:      DefaultMaxElapsedTime,
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}

	operation := func(service v1.Service) func() (err error) {
		return func() (err error) {
			errMessage := fmt.Sprintf("Loadbalancer for service %s is not initialised", service.ObjectMeta.Name)
			if service.Status.LoadBalancer.Ingress == nil {
				r.Log.Info(errMessage)
				return fmt.Errorf(errMessage)
			}
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				if ingress.Hostname == "" {
					return fmt.Errorf(errMessage)
				}
			}
			return nil
		}
	}(service)
	err = backoff.Retry(operation, b)
	return err
}

func (r *VpcEndpointServiceReconciler) describeLoadbalancers() (*elbv2.DescribeLoadBalancersOutput, error) {
	input := &elbv2.DescribeLoadBalancersInput{}
	return r.ElbSvc.DescribeLoadBalancers(input)
}

func getLoadBalancerByHostname(hostname string, loadBalancers *elbv2.DescribeLoadBalancersOutput) (*elbv2.LoadBalancer, error) {
	for _, lb := range loadBalancers.LoadBalancers {
		if *lb.DNSName == hostname {
			return lb, nil
		}
	}
	return nil, fmt.Errorf("LB with hostname %s not found on AWS", hostname)
}

func getLoadBalancerARN(lb *elbv2.LoadBalancer) (*string, error) {
	if *lb.LoadBalancerArn == "" {
		return nil, fmt.Errorf("%s ARN is empty", *lb.LoadBalancerName)
	}
	return lb.LoadBalancerArn, nil
}

func getLoadBalancerARNs(lbs []*elbv2.LoadBalancer) ([]*string, error) {
	var arns []*string
	for _, lb := range lbs {
		arn, err := getLoadBalancerARN(lb)
		if err != nil {
			return nil, err
		}
		arns = append(arns, arn)
	}
	return arns, nil
}

func (r *VpcEndpointServiceReconciler) getSvcLoadBalancersFromAWS(svc v1.Service) (loadBalancers []*elbv2.LoadBalancer, err error) {
	awsLoadBalancers, err := r.describeLoadbalancers()
	if err != nil {
		r.Log.Info(fmt.Sprintf("error describing LoadBalancers: %s", err.Error()))
		return nil, fmt.Errorf("error describing LoadBalancers: %s", err.Error())
	}
	for _, lb := range svc.Status.LoadBalancer.Ingress {
		loadBalancer, err := getLoadBalancerByHostname(lb.Hostname, awsLoadBalancers)
		if err != nil {
			r.Log.Info(fmt.Sprintf("LB with name %s not found on AWS", err.Error()))
		}
		loadBalancers = append(loadBalancers, loadBalancer)
	}
	if len(loadBalancers) == 0 {
		return nil, fmt.Errorf("no matching LBs found for %s", svc.Name)
	}
	return loadBalancers, nil
}

func (r *VpcEndpointServiceReconciler) createVpcEndpointService(lbARNs []*string) (*ec2.CreateVpcEndpointServiceConfigurationOutput, error) {
	clientToken := uuid.New().String()
	AcceptanceRequired := false

	input := ec2.CreateVpcEndpointServiceConfigurationInput{
		AcceptanceRequired:      &AcceptanceRequired,
		ClientToken:             &clientToken,
		NetworkLoadBalancerArns: lbARNs,
	}
	return r.Ec2Svc.CreateVpcEndpointServiceConfiguration(&input)
}

func (r *VpcEndpointServiceReconciler) createVpcEndpoint(vpcID, serviceName *string) (*ec2.CreateVpcEndpointOutput, error) {
	clientToken := uuid.New().String()
	privateDNSEnabled := false
	subnetIds := []*string{}
	VpcEndpointType := "Interface"
	filterName := "vpc-id"
	filterValue := []*string{vpcID}
	filter := ec2.Filter{
		Name:   &filterName,
		Values: filterValue,
	}

	b := &backoff.ExponentialBackOff{
		InitialInterval:     DefaultInitialInterval,
		RandomizationFactor: DefaultRandomizationFactor,
		Multiplier:          DefaultMultiplier,
		MaxInterval:         DefaultMaxInterval,
		MaxElapsedTime:      DefaultMaxElapsedTime,
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}

	describeSubnetsInput := ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{&filter},
	}

	subnets, err := r.Ec2Svc.DescribeSubnets(&describeSubnetsInput)
	if err != nil {
		return nil, err
	}

	for _, subnet := range subnets.Subnets {
		subnetIds = append(subnetIds, subnet.SubnetId)
	}

	createVpcEndpointInput := ec2.CreateVpcEndpointInput{
		ClientToken:       &clientToken,
		PrivateDnsEnabled: &privateDNSEnabled,
		ServiceName:       serviceName,
		SubnetIds:         subnetIds,
		VpcEndpointType:   &VpcEndpointType,
		VpcId:             vpcID,
	}
	var output *ec2.CreateVpcEndpointOutput
	operation := func(createVpcEndpointInput *ec2.CreateVpcEndpointInput) func() (err error) {
		return func() (err error) {
			output, err = r.Ec2Svc.CreateVpcEndpoint(createVpcEndpointInput)
			if err != nil {
				if strings.Contains(err.Error(), "Unavailable: The service is unavailable. Please try again shortly.") {
					r.Log.Info(err.Error())
					return err
				}
				return backoff.Permanent(err)
			}
			return nil
		}
	}(&createVpcEndpointInput)
	err = backoff.Retry(operation, b)
	if err != nil {
		return nil, err
	}
	return output, nil
}

func (r *VpcEndpointServiceReconciler) waitForVpcEndpointService(vpcEndpointServiceID *string) (err error) {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     DefaultInitialInterval,
		RandomizationFactor: DefaultRandomizationFactor,
		Multiplier:          DefaultMultiplier,
		MaxInterval:         DefaultMaxInterval,
		MaxElapsedTime:      DefaultMaxElapsedTime,
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}

	operation := func(vpcEndpointServiceID *string) func() (err error) {
		return func() (err error) {
			describeVpcEndpointServiceConfigurationsInput := ec2.DescribeVpcEndpointServiceConfigurationsInput{
				ServiceIds: []*string{vpcEndpointServiceID},
			}

			vpcEndpointServiceConfigurationOutput, err := r.Ec2Svc.DescribeVpcEndpointServiceConfigurations(&describeVpcEndpointServiceConfigurationsInput)

			var vpcEndpointServiceState string
			for _, endpointService := range vpcEndpointServiceConfigurationOutput.ServiceConfigurations {
				vpcEndpointServiceState = *endpointService.ServiceState
				errMessage := fmt.Sprintf("Vpc endpoint service is %s", vpcEndpointServiceState)
				if strings.ToLower(vpcEndpointServiceState) != "available" {
					r.Log.Info(errMessage)
					return fmt.Errorf(errMessage)
				}
			}
			return nil
		}
	}(vpcEndpointServiceID)
	err = backoff.Retry(operation, b)
	return err
}

func (r *VpcEndpointServiceReconciler) waitForVpcEndpoint(vpcEndpointid *string) (err error) {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     DefaultInitialInterval,
		RandomizationFactor: DefaultRandomizationFactor,
		Multiplier:          DefaultMultiplier,
		MaxInterval:         DefaultMaxInterval,
		MaxElapsedTime:      DefaultMaxElapsedTime,
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}

	operation := func(vpcEndpointid *string) func() (err error) {
		return func() (err error) {

			describeVpcEndpointsInput := ec2.DescribeVpcEndpointsInput{
				VpcEndpointIds: []*string{vpcEndpointid},
			}
			vpcEndpointsOutput, err := r.Ec2Svc.DescribeVpcEndpoints(&describeVpcEndpointsInput)

			var vpcEndpointState string

			for _, endpoint := range vpcEndpointsOutput.VpcEndpoints {
				errMessage := fmt.Sprintf("VPC Endpoint %s is %s", *endpoint.VpcEndpointId, *endpoint.State)
				if strings.ToLower(vpcEndpointState) != "available" {
					r.Log.Info(errMessage)
					return fmt.Errorf(errMessage)
				}
			}
			return nil
		}
	}(vpcEndpointid)
	err = backoff.Retry(operation, b)
	return err
}

//Variadic function ;)
func (r *VpcEndpointServiceReconciler) handleReconcileError(format string, params ...interface{}) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf(format, params...))
	// We don't return error as this makes controller to re-queue
	return ctrl.Result{}, nil
}

func (r *VpcEndpointServiceReconciler) createDNSRecord(hostname, hostedZoneID *string, vpcEndpoint *ec2.CreateVpcEndpointOutput) (*route53.ChangeResourceRecordSetsOutput, error) {

	input := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String("CREATE"),
					ResourceRecordSet: &route53.ResourceRecordSet{
						AliasTarget: &route53.AliasTarget{
							DNSName:              aws.String(*vpcEndpoint.VpcEndpoint.DnsEntries[0].DnsName),
							EvaluateTargetHealth: aws.Bool(false),
							HostedZoneId:         aws.String(*vpcEndpoint.VpcEndpoint.DnsEntries[0].HostedZoneId),
						},
						Name: aws.String(*hostname),
						Type: aws.String("A"),
					},
				},
			},
		},
		HostedZoneId: aws.String(*hostedZoneID),
	}
	return r.Route53Svc.ChangeResourceRecordSets(input)
}

func (r *VpcEndpointServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := v1.Service{}
	err := r.Client.Get(ctx, req.NamespacedName, &svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	var vpcID, hostname, hostedZoneID string
	for key, value := range svc.Annotations {
		if key == endpointAnnotationVpcID {
			vpcID = value
		}
		if key == endpointAnnotationHostname {
			hostname = value
		}
		if key == endpointAnnotationHostedZoneID {
			hostedZoneID = value
		}
	}

	err = r.waitForLoadBalancer(svc)
	if err != nil {
		return r.handleReconcileError("error waiting for LB to initialise: %s. giving up after %s", err.Error(), DefaultMaxElapsedTime.String())
	}
	loadBalancers, err := r.getSvcLoadBalancersFromAWS(svc)
	if err != nil {
		return r.handleReconcileError("error: can't provision endpoint for %s load balancer. %s", svc.Name, err.Error())
	}
	arns, err := getLoadBalancerARNs(loadBalancers)
	if err != nil {
		return r.handleReconcileError("error getting load balancer ARN, %s", err.Error())
	}
	endpointService, err := r.createVpcEndpointService(arns)
	if err != nil {
		return r.handleReconcileError("error creating VPC endpoint service, %s, service name: %s", err.Error(), svc.Name)
	}
	err = r.waitForVpcEndpointService(endpointService.ServiceConfiguration.ServiceId)
	if err != nil {
		return r.handleReconcileError("error waiting for VPC endpoint service to initialise: %s. giving up after %s", err.Error(), DefaultMaxElapsedTime.String())
	}
	r.Log.Info(fmt.Sprintf("Endpoint service %s created", *endpointService.ServiceConfiguration.ServiceName))

	if vpcID == "" {
		return r.handleReconcileError("no endpoint vpc id found in annotations, skipping service %s", svc.Name)
	}
	vpcEndpoint, err := r.createVpcEndpoint(&vpcID, endpointService.ServiceConfiguration.ServiceName)
	if err != nil {
		return r.handleReconcileError("error creating VPC endpoint, %s", err.Error())
	}
	err = r.waitForVpcEndpoint(vpcEndpoint.VpcEndpoint.VpcEndpointId)
	if err != nil {
		return r.handleReconcileError("error waiting for VPC endpoint to initialise: %s. giving up after %s", err.Error(), DefaultMaxElapsedTime.String())
	}
	r.Log.Info(fmt.Sprintf("Endpoint %s created", *vpcEndpoint.VpcEndpoint.ServiceName))
	if hostname == "" || hostedZoneID == "" {
		return r.handleReconcileError("no endpoint vpc id found in annotations, skipping service %s", svc.Name)
	}
	os.Exit(0)
	_, err = r.createDNSRecord(&hostname, &hostedZoneID, vpcEndpoint)
	if err != nil {
		return r.handleReconcileError("error creating DNS record for endpoint %s, %s", *vpcEndpoint.VpcEndpoint.VpcEndpointId, err.Error())
	}
	r.Log.Info(fmt.Sprintf("DNS record service %s created", hostname))
	return ctrl.Result{}, nil
}

func (r *VpcEndpointServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controller := ctrl.NewControllerManagedBy(mgr).
		For(&v1.Service{}).
		WithEventFilter(ignoreNonLoadBalancerServicePredicate()).
		Complete(r)
	return controller
}

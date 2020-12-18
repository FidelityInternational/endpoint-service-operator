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
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
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

const (
	DefaultInitialInterval     = 500 * time.Millisecond
	DefaultRandomizationFactor = 0.5
	DefaultMultiplier          = 1.5
	DefaultMaxInterval         = 60 * time.Second
	DefaultMaxElapsedTime      = 1 * time.Second
)

var (
	VpcID = "fake"
)

// VpcEndpointServiceReconciler reconciles a VpcEndpointService object
type VpcEndpointServiceReconciler struct {
	ElbSvc  *elbv2.ELBV2
	Ec2Svc  *ec2.EC2
	VpcID   *string
	Subnets []*ec2.Subnet
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

func NewVpcEndpointServiceReconciler(mgr manager.Manager, log logr.Logger) (*VpcEndpointServiceReconciler, error) {
	session := session.Must(session.NewSession())
	ec2Svc := ec2.New(session)
	filterName := "vpc-id"
	filterValue := []*string{&VpcID}
	filter := ec2.Filter{
		Name:   &filterName,
		Values: filterValue,
	}

	input := ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{&filter},
	}

	subnets, err := ec2Svc.DescribeSubnets(&input)
	if err != nil {
		return nil, err
	}

	return &VpcEndpointServiceReconciler{
		ElbSvc:  elbv2.New(session),
		Ec2Svc:  ec2Svc,
		VpcID:   &VpcID,
		Client:  mgr.GetClient(),
		Log:     log,
		Scheme:  mgr.GetScheme(),
		Subnets: subnets.Subnets,
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
			err_message := fmt.Sprintf("Loadbalancer for service %s is not initialised", service.ObjectMeta.Name)
			if service.Status.LoadBalancer.Ingress == nil {
				r.Log.Info(err_message)
				return fmt.Errorf(err_message)
			}
			for _, ingress := range service.Status.LoadBalancer.Ingress {
				if ingress.Hostname == "" {
					return fmt.Errorf(err_message)
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

func (r *VpcEndpointServiceReconciler) createVpcEndpoint(serviceName *string) (*ec2.CreateVpcEndpointOutput, error) {
	clientToken := uuid.New().String()
	privateDnsEnabled := false
	subnetIds := []*string{}
	VpcEndpointType := "Interface"
	for _, subnet := range r.Subnets {
		subnetIds = append(subnetIds, subnet.SubnetId)
	}
	input := ec2.CreateVpcEndpointInput{
		ClientToken:       &clientToken,
		PrivateDnsEnabled: &privateDnsEnabled,
		ServiceName:       serviceName,
		SubnetIds:         subnetIds,
		VpcEndpointType:   &VpcEndpointType,
		VpcId:             r.VpcID,
	}
	output, err := r.Ec2Svc.CreateVpcEndpoint(&input)
	if err != nil {
		return nil, err
	}
	return output, nil
}

//Variadic function ;)
func (r *VpcEndpointServiceReconciler) handleReconcileError(format string, params ...interface{}) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf(format, params...))
	// We don't return error as this makes controller to re-queue
	return ctrl.Result{}, nil
}

func (r *VpcEndpointServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := v1.Service{}
	err := r.Client.Get(ctx, req.NamespacedName, &svc)
	if err != nil {
		return ctrl.Result{}, err
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
		return r.handleReconcileError("error creating VPC endpoint service, %s", err.Error())
	}
	time.Sleep(5 * time.Second)
	output, err := r.createVpcEndpoint(endpointService.ServiceConfiguration.ServiceName)
	if err != nil {
		return r.handleReconcileError("error creating VPC endpoint service, %s", err.Error())
	}
	r.Log.Info(fmt.Sprintf("Endpoint service %s created", *output.VpcEndpoint.ServiceName))
	return ctrl.Result{}, nil
}

func (r *VpcEndpointServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controller := ctrl.NewControllerManagedBy(mgr).
		For(&v1.Service{}).
		WithEventFilter(ignoreNonLoadBalancerServicePredicate()).
		Complete(r)
	return controller
}

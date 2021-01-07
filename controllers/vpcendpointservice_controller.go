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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/route53"
	backoff "github.com/cenkalti/backoff/v4"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	DefaultMaxElapsedTime          = 900 * time.Second
	endpointAnnotationVpcID        = "vpc-endpoint-service-operator.fil.com/endpoint-vpc-id"
	endpointAnnotationHostname     = "vpc-endpoint-service-operator.fil.com/hostname"
	endpointAnnotationHostedZoneID = "vpc-endpoint-service-operator.fil.com/hosted-zone-id"
	endpointFinalizer              = "vpc-endpoint-service-operator.fil.com/finalizer"
)

// VpcEndpointServiceReconciler reconciles a VpcEndpointService object
type VpcEndpointServiceReconciler struct {
	ElbSvc     *elbv2.ELBV2
	Ec2Svc     *ec2.EC2
	Route53Svc *route53.Route53
	client.Client
	Log     logr.Logger
	Scheme  *runtime.Scheme
	Cluster *eks.Cluster
}

// NewVpcEndpointServiceReconciler creates default reconsiler
func NewVpcEndpointServiceReconciler(mgr manager.Manager, log logr.Logger, clusterName *string) (*VpcEndpointServiceReconciler, error) {
	session := session.Must(session.NewSession())
	ec2Svc := ec2.New(session)
	route53Svc := route53.New(session)
	eksSvc := eks.New(session)
	hostname := mgr.GetConfig().Host
	var cluster *eks.Cluster
	if *clusterName == "" {
		var err error
		cluster, err = getClusterName(&hostname, eksSvc)
		if err != nil {
			cluster = &eks.Cluster{
				Name: &hostname,
			}
		}
	}
	return &VpcEndpointServiceReconciler{
		ElbSvc:     elbv2.New(session),
		Ec2Svc:     ec2Svc,
		Route53Svc: route53Svc,
		Client:     mgr.GetClient(),
		Log:        log,
		Scheme:     mgr.GetScheme(),
		Cluster:    cluster,
	}, nil
}

func getClusterName(hostname *string, svc *eks.EKS) (*eks.Cluster, error) {
	input := &eks.ListClustersInput{}

	clusters, err := svc.ListClusters(input)
	if err != nil {
		return nil, err
	}

	for _, clusterName := range clusters.Clusters {
		input := &eks.DescribeClusterInput{
			Name: aws.String(*clusterName),
		}
		cluster, err := svc.DescribeCluster(input)
		if err != nil {
			return nil, err
		}

		if cluster.Cluster.Endpoint != nil && *cluster.Cluster.Endpoint == *hostname {
			return cluster.Cluster, nil
		}
	}
	return nil, fmt.Errorf("No cluster found on AWS")
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

func (r *VpcEndpointServiceReconciler) waitForLoadBalancer(service *v1.Service) ([]*elbv2.LoadBalancer, error) {
	errMessage := fmt.Sprintf("Loadbalancer for service %s is not initialised", service.ObjectMeta.Name)
	if service.Status.LoadBalancer.Ingress == nil {
		r.Log.Info(errMessage)
		return nil, fmt.Errorf(errMessage)
	}
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.Hostname == "" {
			return nil, fmt.Errorf(errMessage)
		}
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
	var loadbalancers []*elbv2.LoadBalancer
	operation := func(service *v1.Service) func() (err error) {
		return func() (err error) {
			loadbalancers, err = r.getLoadBalancers(service)
			if err != nil {
				return err
			}
			for _, loadbalancer := range loadbalancers {
				if strings.ToLower(*loadbalancer.State.Code) != "active" {
					errMessage := fmt.Sprintf("Loadbalancer %s status is %s", *loadbalancer.LoadBalancerName, *loadbalancer.State.Code)
					r.Log.Info(errMessage)
					return fmt.Errorf(errMessage)
				}
			}
			return nil
		}
	}(service)
	err := backoff.Retry(operation, b)
	return loadbalancers, err
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

func (r *VpcEndpointServiceReconciler) getLoadBalancers(svc *v1.Service) (loadBalancers []*elbv2.LoadBalancer, err error) {
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

func (r *VpcEndpointServiceReconciler) createVpcEndpointService(lbARNs []*string, serviceName *string) (*ec2.ServiceConfiguration, error) {
	clientToken := uuid.New().String()
	AcceptanceRequired := false
	NameKey := "Name"
	ServiceKey := "Service"
	resourceType := "vpc-endpoint-service"
	input := ec2.CreateVpcEndpointServiceConfigurationInput{
		AcceptanceRequired:      &AcceptanceRequired,
		ClientToken:             &clientToken,
		NetworkLoadBalancerArns: lbARNs,
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: &resourceType,
				Tags: []*ec2.Tag{
					{
						Key:   &NameKey,
						Value: r.Cluster.Name,
					},
					{
						Key:   &ServiceKey,
						Value: serviceName,
					},
				},
			},
		},
	}
	output, err := r.Ec2Svc.CreateVpcEndpointServiceConfiguration(&input)
	if err != nil {
		return nil, err
	}
	return output.ServiceConfiguration, nil
}

func (r *VpcEndpointServiceReconciler) getVpcEndpointService(serviceName *string) (*ec2.ServiceConfiguration, error) {
	clusterFilterName := "tag:Name"
	clusterFilterValue := []*string{r.Cluster.Name}
	clusterFilter := ec2.Filter{
		Name:   &clusterFilterName,
		Values: clusterFilterValue,
	}
	serviceFilterName := "tag:Service"
	serviceFilterValue := []*string{serviceName}
	serviceFilter := ec2.Filter{
		Name:   &serviceFilterName,
		Values: serviceFilterValue,
	}
	input := &ec2.DescribeVpcEndpointServiceConfigurationsInput{
		Filters: []*ec2.Filter{&clusterFilter, &serviceFilter},
	}
	vpcEndpointService, err := r.Ec2Svc.DescribeVpcEndpointServiceConfigurations(input)
	if err != nil {
		return nil, err
	}
	if vpcEndpointService.ServiceConfigurations == nil {
		return nil, fmt.Errorf("No VPC Endpoint service for kube service %s found", *serviceName)
	}
	return vpcEndpointService.ServiceConfigurations[0], nil
}

func (r *VpcEndpointServiceReconciler) getVpcEndpoint(serviceName *string) (*ec2.VpcEndpoint, error) {
	filterName := "service-name"
	filterValue := []*string{serviceName}
	filter := ec2.Filter{
		Name:   &filterName,
		Values: filterValue,
	}
	input := &ec2.DescribeVpcEndpointsInput{
		Filters: []*ec2.Filter{&filter},
	}
	output, err := r.Ec2Svc.DescribeVpcEndpoints(input)
	if err != nil {
		return nil, err
	}
	if output.VpcEndpoints == nil || *output.VpcEndpoints[0].State == "deleting" {
		return nil, fmt.Errorf("No VPC Endpoint for service %s found", *serviceName)
	}
	return output.VpcEndpoints[0], nil
}

func (r *VpcEndpointServiceReconciler) getDNSRecords(hostname, hostedZoneID *string) ([]*route53.ResourceRecordSet, error) {
	l := len(*hostname)
	h := *hostname
	var fullyQualifiedHostname string
	if string(h[l-1]) != "." {
		fullyQualifiedHostname = fmt.Sprintf("%s.", *hostname)
	}

	input := &route53.ListResourceRecordSetsInput{
		HostedZoneId: hostedZoneID,
	}

	output, err := r.Route53Svc.ListResourceRecordSets(input)
	if err != nil {
		return nil, err
	}

	records := []*route53.ResourceRecordSet{}

	for _, record := range output.ResourceRecordSets {
		if *record.Name == fullyQualifiedHostname && *record.Type == "A" {
			records = append(records, record)
		}
		if *record.Name == fullyQualifiedHostname && *record.Type == "TXT" {
			records = append(records, record)
		}
	}
	return records, nil
}

func (r *VpcEndpointServiceReconciler) createVpcEndpoint(vpcID, serviceName *string, vpcEndpointService *ec2.ServiceConfiguration) (*ec2.VpcEndpoint, error) {
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
	NameKey := "Name"
	ServiceKey := "Service"
	resourceType := "vpc-endpoint"
	createVpcEndpointInput := ec2.CreateVpcEndpointInput{
		ClientToken:       &clientToken,
		PrivateDnsEnabled: &privateDNSEnabled,
		ServiceName:       vpcEndpointService.ServiceName,
		SubnetIds:         subnetIds,
		VpcEndpointType:   &VpcEndpointType,
		VpcId:             vpcID,
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: &resourceType,
				Tags: []*ec2.Tag{
					{
						Key:   &NameKey,
						Value: r.Cluster.Name,
					},
					{
						Key:   &ServiceKey,
						Value: serviceName,
					},
				},
			},
		},
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
	return output.VpcEndpoint, nil
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
			for _, endpoint := range vpcEndpointsOutput.VpcEndpoints {
				if strings.ToLower(*endpoint.State) != "available" {
					errMessage := fmt.Sprintf("VPC Endpoint %s status is %s", *endpoint.VpcEndpointId, *endpoint.State)
					r.Log.Info(errMessage)
					return fmt.Errorf(errMessage)
				}
				if endpoint.DnsEntries[0].DnsName == nil || *endpoint.DnsEntries[0].DnsName == "" {
					errMessage := fmt.Sprintf("VPC Endpoint %s DNS record is being provisioned", *endpoint.VpcEndpointId)
					r.Log.Info(errMessage)
					return fmt.Errorf(errMessage)
				}
			}
			return nil
		}
	}(vpcEndpointid)
	return backoff.Retry(operation, b)
}

func (r *VpcEndpointServiceReconciler) createDNSRecord(hostname, hostedZoneID, serviceName *string, vpcEndpoint *ec2.VpcEndpoint) (*route53.ChangeResourceRecordSetsOutput, error) {
	tags := fmt.Sprintf("\"Owner=%s;Service=%s\"", *r.Cluster.Name, *serviceName)
	input := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String("CREATE"),
					ResourceRecordSet: &route53.ResourceRecordSet{
						AliasTarget: &route53.AliasTarget{
							DNSName:              aws.String(*vpcEndpoint.DnsEntries[0].DnsName),
							EvaluateTargetHealth: aws.Bool(false),
							HostedZoneId:         aws.String(*vpcEndpoint.DnsEntries[0].HostedZoneId),
						},
						Name: aws.String(*hostname),
						Type: aws.String("A"),
					},
				},
				{
					Action: aws.String("CREATE"),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(*hostname),
						Type: aws.String("TXT"),
						TTL:  aws.Int64(30),
						ResourceRecords: []*route53.ResourceRecord{
							{
								Value: aws.String(tags),
							},
						},
					},
				},
			},
		},
		HostedZoneId: aws.String(*hostedZoneID),
	}
	return r.Route53Svc.ChangeResourceRecordSets(input)
}

func (r *VpcEndpointServiceReconciler) finalizeEndpoint(svc *v1.Service) error {
	return nil
}

func (r *VpcEndpointServiceReconciler) updateEndpoint(svc *v1.Service) error {
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

	loadbalancers, err := r.waitForLoadBalancer(svc)
	if err != nil {
		return err
	}
	arns, err := getLoadBalancerARNs(loadbalancers)
	if err != nil {
		return fmt.Errorf("error getting load balancer ARN, %s", err.Error())
	}

	vpcEndpointService, err := r.getVpcEndpointService(&svc.Name)
	if vpcEndpointService == nil || err != nil {
		vpcEndpointService, err = r.createVpcEndpointService(arns, &svc.Name)
		if err != nil {
			return fmt.Errorf("error creating VPC endpoint service, %s, service name: %s", err.Error(), svc.Name)
		}
		err = r.waitForVpcEndpointService(vpcEndpointService.ServiceId)
		if err != nil {
			return fmt.Errorf("error waiting for VPC endpoint service to initialise: %s. giving up after %s", err.Error(), DefaultMaxElapsedTime.String())
		}
		r.Log.Info(fmt.Sprintf("VPC Endpoint service %s for kube service %s created", *vpcEndpointService.ServiceName, svc.Name))
	} else {
		r.Log.Info(fmt.Sprintf("VPC Endpoint service %s for kube service %s already exists", *vpcEndpointService.ServiceId, svc.Name))
	}
	if vpcID == "" {
		return fmt.Errorf("no VPC ID found in annotations, skipping kube service %s", svc.Name)
	}

	vpcEndpoint, err := r.getVpcEndpoint(vpcEndpointService.ServiceName)
	if vpcEndpoint == nil || err != nil {
		vpcEndpoint, err = r.createVpcEndpoint(&vpcID, &svc.Name, vpcEndpointService)
		if err != nil {
			return fmt.Errorf("error creating VPC endpoint, %s", err.Error())
		}
		err = r.waitForVpcEndpoint(vpcEndpoint.VpcEndpointId)
		if err != nil {
			return fmt.Errorf("error waiting for VPC endpoint to initialise: %s. giving up after %s", err.Error(), DefaultMaxElapsedTime.String())
		}
		r.Log.Info(fmt.Sprintf("VPC Endpoint %s for kube service %s created", *vpcEndpoint.VpcEndpointId, svc.Name))
	} else {
		r.Log.Info(fmt.Sprintf("VPC Endpoint %s for kube service %s already exists", *vpcEndpoint.VpcEndpointId, svc.Name))
	}
	if hostname == "" || hostedZoneID == "" {
		return fmt.Errorf("no VPC ID found in annotations, skipping kube service %s", svc.Name)
	}

	records, err := r.getDNSRecords(&hostname, &hostedZoneID)
	if err != nil {
		return fmt.Errorf("error getting DNS records for VPC endpoint %s, %s", *vpcEndpoint.VpcEndpointId, err.Error())
	}
	if len(records) == 0 {
		_, err = r.createDNSRecord(&hostname, &hostedZoneID, &svc.Name, vpcEndpoint)
		if err != nil {
			return fmt.Errorf("error creating DNS records for VPC endpoint %s, %s", *vpcEndpoint.VpcEndpointId, err.Error())
		}
		r.Log.Info(fmt.Sprintf("DNS records for %s for kube service %s created", hostname, svc.Name))
	} else {
		r.Log.Info(fmt.Sprintf("DNS records for %s for kube service %s already exist", hostname, svc.Name))
	}
	return nil
}

// Reconcile runs all logic related to the creation of endpoint services, endpoints and DNS records
func (r *VpcEndpointServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := &v1.Service{}
	if err := r.Client.Get(ctx, req.NamespacedName, svc); err != nil {
		if errors.IsNotFound(err) {
			r.Log.Info(fmt.Sprintf("Service resource %s not found. Ignoring since object must be deleted.", svc.Name))
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	isServiceMarkedToBeDeleted := svc.GetDeletionTimestamp() != nil
	if isServiceMarkedToBeDeleted {
		if contains(svc.GetFinalizers(), endpointFinalizer) {
			if err := r.finalizeEndpoint(svc); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(svc, endpointFinalizer)
			err := r.Update(ctx, svc)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !contains(svc.GetFinalizers(), endpointFinalizer) {
		if err := r.addFinalizer(svc); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.updateEndpoint(svc); err != nil {
		return ctrl.Result{
			RequeueAfter: 1 * time.Second,
		}, err
	}
	return ctrl.Result{}, nil
}

func (r *VpcEndpointServiceReconciler) addFinalizer(svc *v1.Service) error {
	r.Log.Info(fmt.Sprintf("Adding Finalizer for the %s service", svc.Name))
	controllerutil.AddFinalizer(svc, endpointFinalizer)

	err := r.Update(context.TODO(), svc)
	if err != nil {
		r.Log.Error(err, fmt.Sprintf("Failed Adding Finalizer for the %s service", svc.Name))
		return err
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// SetupWithManager configures reconciler
func (r *VpcEndpointServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	controller := ctrl.NewControllerManagedBy(mgr).
		For(&v1.Service{}).
		WithEventFilter(ignoreNonLoadBalancerServicePredicate()).
		Complete(r)
	return controller
}

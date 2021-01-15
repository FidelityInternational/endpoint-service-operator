# endpoint-service-operator POC

![](img/Sir_Topham_Hatt_1986.jpg)

## disclaimer

This is POC project. you can use it on your own risk

## running

make sure you have proper kubernetes and AWS creds set up.
```
make run
```

will start the operator. No coompiled or dockerized versions are available yet

## supported annotations
```
vpc-endpoint-service-operator.fil.com/endpoint-vpc-id
```
A VPC ID that will host the endpoint

```
vpc-endpoint-service-operator.fil.com/hosted-zone-id
```
Hosted Zone ID that will be used to provision DNS records

```
vpc-endpoint-service-operator.fil.com/hostname
```
Endpont FQDN

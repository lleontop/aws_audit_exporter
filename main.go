// Copyright 2016 Qubit Group
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	region  = flag.String("region", "eu-west-1", "the region to query")
	taglist = flag.String("instance-tags", "", "comma seperated list of tag keys to use as metric labels")
	dur     = flag.Duration("duration", time.Minute*4, "How often to query the API")
	addr    = flag.String("addr", ":9190", "port to listen on")

	riLabels = []string{
		"az",
		"scope",
		"tenancy",
		"instance_type",
		"offer_type",
		"product",
	}
	riInstanceCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aws_ec2_reserved_instances_count",
		Help: "Number of reserved instances in this reservation",
	},
		riLabels)

	rilLabels = []string{
		"az",
		"scope",
		"instance_type",
		"product",
		"state",
	}

	rilInstanceCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aws_ec2_reserved_instances_listing_count",
		Help: "Number of reserved instances listed on the market for a reservation",
	},
		rilLabels)

	instancesLabels = []string{
		"groups",
		"owner_id",
		"requester_id",
		"az",
		"instance_type",
		"lifecycle",
	}

	siLabels = []string{
		"az",
		"product",
		"persistence",
		"instance_type",
		"launch_group",
		"instance_profile",
	}

	sphLabels = []string{
		"az",
		"product",
		"instance_type",
	}

	sphPrice = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aws_ec2_spot_price_per_hour_dollars",
		Help: "Current market price of a spot instance, per hour,  in dollars",
	},
		sphLabels)
)

// We have to construct the set of tags for this based on the program
// args, so it is created in main
var instancesCount *prometheus.GaugeVec
var instanceTags = map[string]string{}

// Similarly, we want to use the instance labels in the spot instance
// metrics
var siCount *prometheus.GaugeVec
var siBidPrice *prometheus.GaugeVec
var siBlockHourlyPrice *prometheus.GaugeVec

// We'll cache the instance tag labels so that we can use them to separate
// out spot instance spend
var instanceLabelsCacheMutex = sync.RWMutex{}
var instanceLabelsCache = map[string]prometheus.Labels{}
var instanceLabelsCacheIsVPC = map[string]bool{}

func main() {
	flag.Parse()

	tagl := []string{}
	for _, tstr := range strings.Split(*taglist, ",") {
		ctag := tagname(tstr)
		instanceTags[tstr] = ctag
		tagl = append(tagl, ctag)
	}
	instancesCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aws_ec2_instances_count",
		Help: "End time of this reservation",
	},
		append(instancesLabels, tagl...))

	siCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aws_ec2_spot_request_count",
		Help: "Number of active/fullfilled spot requests",
	},
		append(siLabels, tagl...))
	siBidPrice = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aws_ec2_spot_request_bid_price_hourly_dollars",
		Help: "cost of spot instances hourly usage in dollars",
	},
		append(siLabels, tagl...))
	siBlockHourlyPrice = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aws_ec2_spot_request_actual_block_price_hourly_dollars",
		Help: "fixed hourly cost of limited duration spot instances in dollars",
	},
		append(siLabels, tagl...))

	prometheus.Register(instancesCount)
	prometheus.Register(riInstanceCount)
	prometheus.Register(rilInstanceCount)
	prometheus.Register(siCount)
	prometheus.Register(siBidPrice)
	prometheus.Register(siBlockHourlyPrice)
	prometheus.Register(sphPrice)

	sess, err := session.NewSession()
	if err != nil {
		log.Fatalf("failed to create session %v\n", err)
	}

	svc := ec2.New(sess, &aws.Config{Region: aws.String(*region)})

	go func() {
		for {
			instances(svc, *region)
			go reservations(svc, *region)
			go spots(svc, *region)
			<-time.After(*dur)
		}
	}()

	http.Handle("/metrics", prometheus.Handler())

	log.Println(http.ListenAndServe(*addr, nil))
}
func instances(svc *ec2.EC2, awsRegion string) {
	instanceLabelsCacheMutex.Lock()
	defer instanceLabelsCacheMutex.Unlock()

	//Clear the cache
	instanceLabelsCache = map[string]prometheus.Labels{}
	instanceLabelsCacheIsVPC = map[string]bool{}

	params := &ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("instance-state-code"),
				Values: []*string{aws.String("16")},
			},
		},
	}
	resp, err := svc.DescribeInstances(params)
	if err != nil {
		fmt.Println("there was an error listing instances in", awsRegion, err.Error())
		log.Fatal(err.Error())
	}

	instancesCount.Reset()
	labels := prometheus.Labels{}
	for _, r := range resp.Reservations {
		groups := []string{}
		for _, g := range r.Groups {
			groups = append(groups, *g.GroupName)
		}
		sort.Strings(groups)
		labels["groups"] = strings.Join(groups, ",")
		labels["owner_id"] = *r.OwnerId
		labels["requester_id"] = *r.OwnerId
		if r.RequesterId != nil {
			labels["requester_id"] = *r.RequesterId
		}
		for _, ins := range r.Instances {
			labels["az"] = *ins.Placement.AvailabilityZone
			labels["instance_type"] = *ins.InstanceType
			labels["lifecycle"] = "normal"
			if ins.InstanceLifecycle != nil {
				labels["lifecycle"] = *ins.InstanceLifecycle
			}
			instanceLabelsCache[*ins.InstanceId] = prometheus.Labels{}
			for _, label := range instanceTags {
				labels[label] = ""
				instanceLabelsCache[*ins.InstanceId][label] = ""
			}
			for _, tag := range ins.Tags {
				label, ok := instanceTags[*tag.Key]
				if ok {
					labels[label] = *tag.Value
					instanceLabelsCache[*ins.InstanceId][label] = *tag.Value
				}
			}
			if ins.VpcId != nil {
				instanceLabelsCacheIsVPC[*ins.InstanceId] = true
			}
			instancesCount.With(labels).Inc()
		}
	}
}

func reservations(svc *ec2.EC2, awsRegion string) {
	instanceLabelsCacheMutex.RLock()
	defer instanceLabelsCacheMutex.RUnlock()

	labels := prometheus.Labels{}
	riInstanceCount.Reset()
	for iid, ils := range instanceLabelsCache {
		labels["scope"] = ils["Availability Zone"]
		labels["az"] = ils["az"]
		labels["instance_type"] = ils["instance_type"]
		labels["tenancy"] = "default"
		labels["offer_type"] = "No Upfront"
		labels["product"] = "Linux/UNIX"
		if _, ok := instanceLabelsCacheIsVPC[iid]; ok {
			labels["product"] += " (Amazon VPC)"
		}
		riInstanceCount.With(labels).Set(0)

		labels["scope"] = ils["Region"]
		labels["az"] = ils["none"]
		riInstanceCount.With(labels).Set(0)
	}

	params := &ec2.DescribeReservedInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("state"),
				Values: []*string{aws.String("active")},
			},
		},
	}
	resp, err := svc.DescribeReservedInstances(params)
	if err != nil {
		fmt.Println("there was an error listing instances in", awsRegion, err.Error())
		log.Fatal(err.Error())
	}

	ris := map[string]*ec2.ReservedInstances{}
	labels = prometheus.Labels{}
	for _, r := range resp.ReservedInstances {
		labels["scope"] = *r.Scope
		if *r.Scope == "Region" {
			labels["az"] = "none"
		} else {
			labels["az"] = *r.AvailabilityZone
		}
		labels["instance_type"] = *r.InstanceType
		labels["tenancy"] = *r.InstanceTenancy
		labels["offer_type"] = *r.OfferingType
		labels["product"] = *r.ProductDescription
		ris[*r.ReservedInstancesId] = r

		riInstanceCount.With(labels).Add(float64(*r.InstanceCount))
	}

	rilparams := &ec2.DescribeReservedInstancesListingsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("status"),
				Values: []*string{aws.String("active")},
			},
		},
	}
	rilresp, err := svc.DescribeReservedInstancesListings(rilparams)
	if err != nil {
		fmt.Println("there was an error listing reserved instances listings in", awsRegion, err.Error())
		log.Fatal(err.Error())
	}
	rilInstanceCount.Reset()

	labels = prometheus.Labels{}
	for _, r := range ris {
		labels["scope"] = *r.Scope
		if *r.Scope == "Region" {
			labels["az"] = "none"
		} else {
			labels["az"] = *r.AvailabilityZone
		}
		labels["instance_type"] = *r.InstanceType
		labels["product"] = *r.ProductDescription

		for _, s := range []string{"available", "sold", "cancelled", "pending"} {
			labels["state"] = s
			rilInstanceCount.With(labels).Set(0)
		}
	}

	labels = prometheus.Labels{}
	for _, ril := range rilresp.ReservedInstancesListings {
		r, ok := ris[*ril.ReservedInstancesId]
		if !ok {
			fmt.Printf("Reservations listing for unknown reservation")
			continue
		}
		labels["scope"] = *r.Scope
		if *r.Scope == "Region" {
			labels["az"] = "none"
		} else {
			labels["az"] = *r.AvailabilityZone
		}
		labels["instance_type"] = *r.InstanceType
		labels["product"] = *r.ProductDescription

		for _, ic := range ril.InstanceCounts {
			labels["state"] = *ic.State
			rilInstanceCount.With(labels).Add(float64(*ic.InstanceCount))
		}
	}
}

func spots(svc *ec2.EC2, awsRegion string) {
	instanceLabelsCacheMutex.RLock()
	defer instanceLabelsCacheMutex.RUnlock()

	params := &ec2.DescribeSpotInstanceRequestsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("state"),
				Values: []*string{aws.String("active")},
			},
		},
	}
	resp, err := svc.DescribeSpotInstanceRequests(params)
	if err != nil {
		fmt.Println("there was an error listing spot requests", awsRegion, err.Error())
		log.Fatal(err.Error())
	}

	productSeen := map[string]bool{}

	labels := prometheus.Labels{}
	siCount.Reset()
	siBlockHourlyPrice.Reset()
	siBidPrice.Reset()
	for _, r := range resp.SpotInstanceRequests {
		for _, label := range instanceTags {
			labels[label] = ""
		}
		if r.InstanceId != nil {
			if ilabels, ok := instanceLabelsCache[*r.InstanceId]; ok {
				for k, v := range ilabels {
					labels[k] = v
				}
			}
		}

		labels["az"] = *r.LaunchedAvailabilityZone

		product := *r.ProductDescription
		if isVpc, ok := instanceLabelsCacheIsVPC[*r.InstanceId]; ok && isVpc {
			product += " (Amazon VPC)"
		}
		labels["product"] = product
		productSeen[product] = true

		labels["persistence"] = "one-time"
		if r.Type != nil {
			labels["persistence"] = *r.Type
		}

		labels["launch_group"] = "none"
		if r.LaunchGroup != nil {
			labels["launch_group"] = *r.LaunchGroup
		}

		labels["instance_type"] = "unknown"
		if r.LaunchSpecification != nil && r.LaunchSpecification.InstanceType != nil {
			labels["instance_type"] = *r.LaunchSpecification.InstanceType
		}

		labels["instance_profile"] = "unknown"
		if r.LaunchSpecification != nil && r.LaunchSpecification.IamInstanceProfile != nil {
			if r.LaunchSpecification.IamInstanceProfile.Name != nil {
				labels["instance_profile"] = *r.LaunchSpecification.IamInstanceProfile.Name
			} else {
				labels["instance_profile"] = *r.LaunchSpecification.IamInstanceProfile.Arn
			}
		}

		price := 0.0
		if r.ActualBlockHourlyPrice != nil {
			if f, err := strconv.ParseFloat(*r.ActualBlockHourlyPrice, 64); err == nil {
				price = f
			}
		}
		siBlockHourlyPrice.With(labels).Add(price)

		price = 0
		if r.SpotPrice != nil {
			if f, err := strconv.ParseFloat(*r.SpotPrice, 64); err == nil {
				price = f
			}
		}
		siBidPrice.With(labels).Add(price)

		siCount.With(labels).Inc()
	}

	// This is silly, but spot instances requests don't seem to include the vpc case
	pList := []*string{}
	for p := range productSeen {
		pp := p
		pList = append(pList, &pp)
	}

	phParams := &ec2.DescribeSpotPriceHistoryInput{
		StartTime: aws.Time(time.Now()),
		EndTime:   aws.Time(time.Now()),
		//		ProductDescriptions: pList,
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("product-description"),
				Values: pList,
			},
		},
	}
	phResp, err := svc.DescribeSpotPriceHistory(phParams)
	if err != nil {
		fmt.Println("there was an error listing spot requests", awsRegion, err.Error())
		log.Fatal(err.Error())
	}
	spLabels := prometheus.Labels{}
	for _, sp := range phResp.SpotPriceHistory {
		spLabels["az"] = *sp.AvailabilityZone
		spLabels["product"] = *sp.ProductDescription
		spLabels["instance_type"] = *sp.InstanceType
		if sp.SpotPrice != nil {
			if f, err := strconv.ParseFloat(*sp.SpotPrice, 64); err == nil {
				sphPrice.With(spLabels).Set(f)
			}
		}
	}
}

var cleanre = regexp.MustCompile("[^A-Za-z0-9]")

func tagname(n string) string {
	c := cleanre.ReplaceAllString(n, "_")
	c = strings.ToLower(strings.Trim(c, "_"))
	return "aws_tag_" + c
}

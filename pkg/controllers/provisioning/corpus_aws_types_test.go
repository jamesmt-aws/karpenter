//go:build corpus

/*
Copyright The Kubernetes Authors.

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

// AWS-realistic instance types for the provisioning corpus.
//
// Mirrors pkg/controllers/disruption/corpus_aws_types_test.go: c7i,
// m7i, r7i families at sizes large through 8xlarge, AWS us-east-1
// on-demand prices. Lifted rather than shared because the disruption
// corpus's helper lives in disruption_test and is not importable.

package provisioning_test

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"

	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/fake"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"sigs.k8s.io/karpenter/pkg/test/scenarios"
)

func awsRealisticInstanceTypes() []*cloudprovider.InstanceType {
	type spec struct {
		family string
		size   string
		cpu    int
		memGiB int
		price  float64
	}
	specs := []spec{
		{"c7i", "large", 2, 4, 0.0850},
		{"c7i", "xlarge", 4, 8, 0.1700},
		{"c7i", "2xlarge", 8, 16, 0.3400},
		{"c7i", "4xlarge", 16, 32, 0.6800},
		{"c7i", "8xlarge", 32, 64, 1.3600},
		{"m7i", "large", 2, 8, 0.1008},
		{"m7i", "xlarge", 4, 16, 0.2016},
		{"m7i", "2xlarge", 8, 32, 0.4032},
		{"m7i", "4xlarge", 16, 64, 0.8064},
		{"m7i", "8xlarge", 32, 128, 1.6128},
		{"r7i", "large", 2, 16, 0.1323},
		{"r7i", "xlarge", 4, 32, 0.2646},
		{"r7i", "2xlarge", 8, 64, 0.5292},
		{"r7i", "4xlarge", 16, 128, 1.0584},
		{"r7i", "8xlarge", 32, 256, 2.1168},
	}
	out := make([]*cloudprovider.InstanceType, 0, len(specs))
	for _, s := range specs {
		name := fmt.Sprintf("%s.%s", s.family, s.size)
		resources := corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", s.cpu)),
			corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dGi", s.memGiB)),
			corev1.ResourcePods:   resource.MustParse("110"),
		}
		offerings := []*cloudprovider.Offering{
			{
				Available: true,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone: "us-east-1a",
				}),
				Price: s.price,
			},
			{
				Available: true,
				Requirements: scheduling.NewLabelRequirements(map[string]string{
					v1.CapacityTypeLabelKey:  v1.CapacityTypeOnDemand,
					corev1.LabelTopologyZone: "us-east-1b",
				}),
				Price: s.price,
			},
		}
		out = append(out, fake.NewInstanceType(fake.InstanceTypeOptions{
			Name:             name,
			Architecture:     "amd64",
			OperatingSystems: sets.New(string(corev1.Linux)),
			Resources:        resources,
			Offerings:        offerings,
		}))
	}
	return out
}

func useAWSInstanceTypes() {
	cloudProvider.InstanceTypes = awsRealisticInstanceTypes()
}

func pickAWSInstances() []scenarios.InstanceMeta {
	its := awsRealisticInstanceTypes()
	metas := make([]scenarios.InstanceMeta, 0, len(its))
	for _, it := range its {
		var meta scenarios.InstanceMeta
		for _, off := range it.Offerings {
			ct := off.Requirements.Get(v1.CapacityTypeLabelKey).Any()
			if ct != v1.CapacityTypeOnDemand {
				continue
			}
			zone := off.Requirements.Get(corev1.LabelTopologyZone).Any()
			meta = scenarios.InstanceMeta{
				InstanceType: it.Name,
				CapacityType: ct,
				Zone:         zone,
			}
			break
		}
		if meta.InstanceType != "" {
			metas = append(metas, meta)
		}
	}
	return metas
}

/*
Copyright IBM Corp. 2016-2017 All Rights Reserved.

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

package configtx

import (
	"fmt"

	"github.com/hyperledger/fabric/common/config"
	"github.com/hyperledger/fabric/common/configtx/api"
	"github.com/hyperledger/fabric/common/policies"
	cb "github.com/hyperledger/fabric/protos/common"

	"github.com/golang/protobuf/proto"
)

type configGroupWrapper struct {
	*cb.ConfigGroup
	deserializedValues map[string]proto.Message
}

func newConfigGroupWrapper(group *cb.ConfigGroup) *configGroupWrapper {
	return &configGroupWrapper{
		ConfigGroup:        group,
		deserializedValues: make(map[string]proto.Message),
	}
}

type configResult struct {
	tx            interface{}
	handler       api.Transactional
	policyHandler api.Transactional
	subResults    []*configResult
}

func (cr *configResult) preCommit() error {
	for _, subResult := range cr.subResults {
		err := subResult.preCommit()
		if err != nil {
			return err
		}
	}
	return cr.handler.PreCommit(cr.tx)
}

func (cr *configResult) commit() {
	for _, subResult := range cr.subResults {
		subResult.commit()
	}
	cr.handler.CommitProposals(cr.tx)
	cr.policyHandler.CommitProposals(cr.tx)
}

func (cr *configResult) rollback() {
	for _, subResult := range cr.subResults {
		subResult.rollback()
	}
	cr.handler.RollbackProposals(cr.tx)
	cr.policyHandler.RollbackProposals(cr.tx)
}

// proposeGroup proposes a group configuration with a given handler
// it will in turn recursively call itself until all groups have been exhausted
// at each call, it returns the handler that was passed in, plus any handlers returned
// by recursive calls into proposeGroup
func (cm *configManager) proposeGroup(tx interface{}, name string, group *configGroupWrapper, handler config.ValueProposer, policyHandler policies.Proposer) (*configResult, error) {
	subGroups := make([]string, len(group.Groups))
	i := 0
	for subGroup := range group.Groups {
		subGroups[i] = subGroup
		i++
	}

	logger.Debugf("Beginning new config for channel %s and group %s", cm.current.channelID, name)
	valueDeserializer, subHandlers, err := handler.BeginValueProposals(tx, subGroups)
	if err != nil {
		return nil, err
	}

	subPolicyHandlers, err := policyHandler.BeginPolicyProposals(tx, subGroups)
	if err != nil {
		return nil, err
	}

	if len(subHandlers) != len(subGroups) || len(subPolicyHandlers) != len(subGroups) {
		return nil, fmt.Errorf("Programming error, did not return as many handlers as groups %d vs %d vs %d", len(subHandlers), len(subGroups), len(subPolicyHandlers))
	}

	result := &configResult{
		tx:            tx,
		handler:       handler,
		policyHandler: policyHandler,
		subResults:    make([]*configResult, 0, len(subGroups)),
	}

	for i, subGroup := range subGroups {
		subResult, err := cm.proposeGroup(tx, name+"/"+subGroup, newConfigGroupWrapper(group.Groups[subGroup]), subHandlers[i], subPolicyHandlers[i])
		if err != nil {
			result.rollback()
			return nil, err
		}
		result.subResults = append(result.subResults, subResult)
	}

	for key, value := range group.Values {
		msg, err := valueDeserializer.Deserialize(key, value.Value)
		if err != nil {
			result.rollback()
			return nil, err
		}
		group.deserializedValues[key] = msg
	}

	for key, policy := range group.Policies {
		if err := policyHandler.ProposePolicy(tx, key, policy); err != nil {
			result.rollback()
			return nil, err
		}
	}

	err = result.preCommit()
	if err != nil {
		result.rollback()
		return nil, err
	}

	return result, nil
}

func (cm *configManager) processConfig(channelGroup *cb.ConfigGroup) (*configResult, error) {
	helperGroup := cb.NewConfigGroup()
	helperGroup.Groups[RootGroupKey] = channelGroup
	groupResult, err := cm.proposeGroup(channelGroup, "", newConfigGroupWrapper(helperGroup), cm.initializer.ValueProposer(), cm.initializer.PolicyProposer())
	if err != nil {
		return nil, err
	}

	return groupResult, nil
}

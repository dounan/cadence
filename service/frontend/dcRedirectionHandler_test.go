// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package frontend

import (
	"context"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
	"github.com/uber/cadence/.gen/go/cadence/workflowservicetest"
	"github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/client"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/archiver"
	"github.com/uber/cadence/common/archiver/provider"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/loggerimpl"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/mocks"
	"github.com/uber/cadence/common/service"
	"github.com/uber/cadence/common/service/config"
	"github.com/uber/cadence/common/service/dynamicconfig"
)

type (
	dcRedirectionHandlerSuite struct {
		suite.Suite
		logger                 log.Logger
		domainName             string
		domainID               string
		currentClusterName     string
		alternativeClusterName string
		config                 *Config
		service                service.Service
		domainCache            cache.DomainCache

		controller               *gomock.Controller
		mockDCRedirectionPolicy  *MockDCRedirectionPolicy
		mockClusterMetadata      *mocks.ClusterMetadata
		mockMetadataMgr          *mocks.MetadataManager
		mockClientBean           *client.MockClientBean
		mockFrontendHandler      *MockWorkflowHandler
		mockRemoteFrontendClient *workflowservicetest.MockClient
		mockArchivalMetadata     *archiver.MockArchivalMetadata
		mockArchiverProvider     *provider.MockArchiverProvider

		frontendHandler *WorkflowHandler
		handler         *DCRedirectionHandlerImpl
	}
)

func TestDCRedirectionHandlerSuite(t *testing.T) {
	s := new(dcRedirectionHandlerSuite)
	suite.Run(t, s)
}

func (s *dcRedirectionHandlerSuite) SetupSuite() {
}

func (s *dcRedirectionHandlerSuite) TearDownSuite() {
}

func (s *dcRedirectionHandlerSuite) SetupTest() {
	var err error
	s.logger, err = loggerimpl.NewDevelopment()
	s.Require().NoError(err)
	s.domainName = "some random domain name"
	s.domainID = "some random domain ID"
	s.currentClusterName = cluster.TestCurrentClusterName
	s.alternativeClusterName = cluster.TestAlternativeClusterName
	s.config = NewConfig(dynamicconfig.NewCollection(dynamicconfig.NewNopClient(), s.logger), 0, false)
	s.mockMetadataMgr = &mocks.MetadataManager{}

	s.mockClusterMetadata = &mocks.ClusterMetadata{}
	s.mockClusterMetadata.On("GetCurrentClusterName").Return(s.currentClusterName)
	s.mockClusterMetadata.On("IsGlobalDomainEnabled").Return(true)
	metricsClient := metrics.NewClient(tally.NoopScope, metrics.Frontend)
	s.mockClientBean = &client.MockClientBean{}
	s.controller = gomock.NewController(s.T())
	s.mockRemoteFrontendClient = workflowservicetest.NewMockClient(s.controller)
	s.mockArchivalMetadata = &archiver.MockArchivalMetadata{}
	s.mockArchiverProvider = &provider.MockArchiverProvider{}
	s.mockClientBean.On("GetRemoteFrontendClient", s.alternativeClusterName).Return(s.mockRemoteFrontendClient)
	s.service = service.NewTestService(s.mockClusterMetadata, nil, metricsClient, s.mockClientBean, s.mockArchivalMetadata, s.mockArchiverProvider)

	s.domainCache = cache.NewDomainCache(s.mockMetadataMgr, s.service.GetClusterMetadata(), s.service.GetMetricsClient(), s.service.GetLogger())
	frontendHandler := NewWorkflowHandler(s.service, s.config, s.mockMetadataMgr, nil, nil, nil, nil, nil, s.domainCache)
	frontendHandler.metricsClient = metricsClient
	frontendHandler.startWG.Done()

	s.handler = NewDCRedirectionHandler(frontendHandler, config.DCRedirectionPolicy{})
	s.mockDCRedirectionPolicy = &MockDCRedirectionPolicy{}
	s.mockFrontendHandler = NewMockWorkflowHandler(s.controller)
	s.handler.frontendHandler = s.mockFrontendHandler
	s.handler.redirectionPolicy = s.mockDCRedirectionPolicy
}

func (s *dcRedirectionHandlerSuite) TearDownTest() {
	s.mockMetadataMgr.AssertExpectations(s.T())
	s.mockDCRedirectionPolicy.AssertExpectations(s.T())
	s.mockArchivalMetadata.AssertExpectations(s.T())
	s.mockArchiverProvider.AssertExpectations(s.T())
	s.controller.Finish()
}

func (s *dcRedirectionHandlerSuite) TestDescribeTaskList() {
	apiName := "DescribeTaskList"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.DescribeTaskListRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.DescribeTaskList(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().DescribeTaskList(gomock.Any(), req).Return(&shared.DescribeTaskListResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().DescribeTaskList(gomock.Any(), req).Return(&shared.DescribeTaskListResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestDescribeWorkflowExecution() {
	apiName := "DescribeWorkflowExecution"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.DescribeWorkflowExecutionRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.DescribeWorkflowExecution(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().DescribeWorkflowExecution(gomock.Any(), req).Return(&shared.DescribeWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().DescribeWorkflowExecution(gomock.Any(), req).Return(&shared.DescribeWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestGetWorkflowExecutionHistory() {
	apiName := "GetWorkflowExecutionHistory"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.GetWorkflowExecutionHistoryRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.GetWorkflowExecutionHistory(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().GetWorkflowExecutionHistory(gomock.Any(), req).Return(&shared.GetWorkflowExecutionHistoryResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().GetWorkflowExecutionHistory(gomock.Any(), req).Return(&shared.GetWorkflowExecutionHistoryResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestListArchivedWorkflowExecutions() {
	apiName := "ListArchivedWorkflowExecutions"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.ListArchivedWorkflowExecutionsRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.ListArchivedWorkflowExecutions(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().ListArchivedWorkflowExecutions(gomock.Any(), req).Return(&shared.ListArchivedWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().ListArchivedWorkflowExecutions(gomock.Any(), req).Return(&shared.ListArchivedWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestListClosedWorkflowExecutions() {
	apiName := "ListClosedWorkflowExecutions"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.ListClosedWorkflowExecutionsRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.ListClosedWorkflowExecutions(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().ListClosedWorkflowExecutions(gomock.Any(), req).Return(&shared.ListClosedWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().ListClosedWorkflowExecutions(gomock.Any(), req).Return(&shared.ListClosedWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestListOpenWorkflowExecutions() {
	apiName := "ListOpenWorkflowExecutions"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.ListOpenWorkflowExecutionsRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.ListOpenWorkflowExecutions(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().ListOpenWorkflowExecutions(gomock.Any(), req).Return(&shared.ListOpenWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().ListOpenWorkflowExecutions(gomock.Any(), req).Return(&shared.ListOpenWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestListWorkflowExecutions() {
	apiName := "ListWorkflowExecutions"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.ListWorkflowExecutionsRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.ListWorkflowExecutions(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().ListWorkflowExecutions(gomock.Any(), req).Return(&shared.ListWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().ListWorkflowExecutions(gomock.Any(), req).Return(&shared.ListWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestScanWorkflowExecutions() {
	apiName := "ScanWorkflowExecutions"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.ListWorkflowExecutionsRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.ScanWorkflowExecutions(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().ScanWorkflowExecutions(gomock.Any(), req).Return(&shared.ListWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().ScanWorkflowExecutions(gomock.Any(), req).Return(&shared.ListWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestCountWorkflowExecutions() {
	apiName := "CountWorkflowExecutions"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.CountWorkflowExecutionsRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.CountWorkflowExecutions(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().CountWorkflowExecutions(gomock.Any(), req).Return(&shared.CountWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().CountWorkflowExecutions(gomock.Any(), req).Return(&shared.CountWorkflowExecutionsResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestPollForActivityTask() {
	apiName := "PollForActivityTask"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.PollForActivityTaskRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.PollForActivityTask(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().PollForActivityTask(gomock.Any(), req).Return(&shared.PollForActivityTaskResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().PollForActivityTask(gomock.Any(), req).Return(&shared.PollForActivityTaskResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestPollForDecisionTask() {
	apiName := "PollForDecisionTask"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.PollForDecisionTaskRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.PollForDecisionTask(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().PollForDecisionTask(gomock.Any(), req).Return(&shared.PollForDecisionTaskResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().PollForDecisionTask(gomock.Any(), req).Return(&shared.PollForDecisionTaskResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestQueryWorkflow() {
	apiName := "QueryWorkflow"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.QueryWorkflowRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.QueryWorkflow(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().QueryWorkflow(gomock.Any(), req).Return(&shared.QueryWorkflowResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().QueryWorkflow(gomock.Any(), req).Return(&shared.QueryWorkflowResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRecordActivityTaskHeartbeat() {
	apiName := "RecordActivityTaskHeartbeat"

	s.mockDCRedirectionPolicy.On("WithDomainIDRedirect",
		s.domainID, apiName, mock.Anything).Return(nil).Times(1)

	token, err := s.handler.tokenSerializer.Serialize(&common.TaskToken{
		DomainID: s.domainID,
	})
	s.Nil(err)
	req := &shared.RecordActivityTaskHeartbeatRequest{
		TaskToken: token,
	}
	resp, err := s.handler.RecordActivityTaskHeartbeat(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RecordActivityTaskHeartbeat(gomock.Any(), req).Return(&shared.RecordActivityTaskHeartbeatResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RecordActivityTaskHeartbeat(gomock.Any(), req).Return(&shared.RecordActivityTaskHeartbeatResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRecordActivityTaskHeartbeatByID() {
	apiName := "RecordActivityTaskHeartbeatByID"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.RecordActivityTaskHeartbeatByIDRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.RecordActivityTaskHeartbeatByID(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RecordActivityTaskHeartbeatByID(gomock.Any(), req).Return(&shared.RecordActivityTaskHeartbeatResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RecordActivityTaskHeartbeatByID(gomock.Any(), req).Return(&shared.RecordActivityTaskHeartbeatResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRequestCancelWorkflowExecution() {
	apiName := "RequestCancelWorkflowExecution"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.RequestCancelWorkflowExecutionRequest{
		Domain: common.StringPtr(s.domainName),
	}
	err := s.handler.RequestCancelWorkflowExecution(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RequestCancelWorkflowExecution(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RequestCancelWorkflowExecution(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestResetStickyTaskList() {
	apiName := "ResetStickyTaskList"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.ResetStickyTaskListRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.ResetStickyTaskList(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().ResetStickyTaskList(gomock.Any(), req).Return(&shared.ResetStickyTaskListResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().ResetStickyTaskList(gomock.Any(), req).Return(&shared.ResetStickyTaskListResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestResetWorkflowExecution() {
	apiName := "ResetWorkflowExecution"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.ResetWorkflowExecutionRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.ResetWorkflowExecution(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().ResetWorkflowExecution(gomock.Any(), req).Return(&shared.ResetWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().ResetWorkflowExecution(gomock.Any(), req).Return(&shared.ResetWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondActivityTaskCanceled() {
	apiName := "RespondActivityTaskCanceled"

	s.mockDCRedirectionPolicy.On("WithDomainIDRedirect",
		s.domainID, apiName, mock.Anything).Return(nil).Times(1)

	token, err := s.handler.tokenSerializer.Serialize(&common.TaskToken{
		DomainID: s.domainID,
	})
	s.Nil(err)
	req := &shared.RespondActivityTaskCanceledRequest{
		TaskToken: token,
	}
	err = s.handler.RespondActivityTaskCanceled(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondActivityTaskCanceled(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondActivityTaskCanceled(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondActivityTaskCanceledByID() {
	apiName := "RespondActivityTaskCanceledByID"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.RespondActivityTaskCanceledByIDRequest{
		Domain: common.StringPtr(s.domainName),
	}
	err := s.handler.RespondActivityTaskCanceledByID(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondActivityTaskCanceledByID(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondActivityTaskCanceledByID(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondActivityTaskCompleted() {
	apiName := "RespondActivityTaskCompleted"

	s.mockDCRedirectionPolicy.On("WithDomainIDRedirect",
		s.domainID, apiName, mock.Anything).Return(nil).Times(1)

	token, err := s.handler.tokenSerializer.Serialize(&common.TaskToken{
		DomainID: s.domainID,
	})
	s.Nil(err)
	req := &shared.RespondActivityTaskCompletedRequest{
		TaskToken: token,
	}
	err = s.handler.RespondActivityTaskCompleted(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondActivityTaskCompleted(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondActivityTaskCompleted(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondActivityTaskCompletedByID() {
	apiName := "RespondActivityTaskCompletedByID"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.RespondActivityTaskCompletedByIDRequest{
		Domain: common.StringPtr(s.domainName),
	}
	err := s.handler.RespondActivityTaskCompletedByID(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondActivityTaskCompletedByID(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondActivityTaskCompletedByID(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondActivityTaskFailed() {
	apiName := "RespondActivityTaskFailed"

	s.mockDCRedirectionPolicy.On("WithDomainIDRedirect",
		s.domainID, apiName, mock.Anything).Return(nil).Times(1)

	token, err := s.handler.tokenSerializer.Serialize(&common.TaskToken{
		DomainID: s.domainID,
	})
	s.Nil(err)
	req := &shared.RespondActivityTaskFailedRequest{
		TaskToken: token,
	}
	err = s.handler.RespondActivityTaskFailed(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondActivityTaskFailed(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondActivityTaskFailed(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondActivityTaskFailedByID() {
	apiName := "RespondActivityTaskFailedByID"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.RespondActivityTaskFailedByIDRequest{
		Domain: common.StringPtr(s.domainName),
	}
	err := s.handler.RespondActivityTaskFailedByID(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondActivityTaskFailedByID(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondActivityTaskFailedByID(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondDecisionTaskCompleted() {
	apiName := "RespondDecisionTaskCompleted"

	s.mockDCRedirectionPolicy.On("WithDomainIDRedirect",
		s.domainID, apiName, mock.Anything).Return(nil).Times(1)

	token, err := s.handler.tokenSerializer.Serialize(&common.TaskToken{
		DomainID: s.domainID,
	})
	s.Nil(err)
	req := &shared.RespondDecisionTaskCompletedRequest{
		TaskToken: token,
	}
	resp, err := s.handler.RespondDecisionTaskCompleted(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondDecisionTaskCompleted(gomock.Any(), req).Return(&shared.RespondDecisionTaskCompletedResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondDecisionTaskCompleted(gomock.Any(), req).Return(&shared.RespondDecisionTaskCompletedResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondDecisionTaskFailed() {
	apiName := "RespondDecisionTaskFailed"

	s.mockDCRedirectionPolicy.On("WithDomainIDRedirect",
		s.domainID, apiName, mock.Anything).Return(nil).Times(1)

	token, err := s.handler.tokenSerializer.Serialize(&common.TaskToken{
		DomainID: s.domainID,
	})
	s.Nil(err)
	req := &shared.RespondDecisionTaskFailedRequest{
		TaskToken: token,
	}
	err = s.handler.RespondDecisionTaskFailed(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondDecisionTaskFailed(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondDecisionTaskFailed(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestRespondQueryTaskCompleted() {
	apiName := "RespondQueryTaskCompleted"

	s.mockDCRedirectionPolicy.On("WithDomainIDRedirect",
		s.domainID, apiName, mock.Anything).Return(nil).Times(1)

	token, err := s.handler.tokenSerializer.SerializeQueryTaskToken(&common.QueryTaskToken{
		DomainID: s.domainID,
	})
	req := &shared.RespondQueryTaskCompletedRequest{
		TaskToken: token,
	}
	err = s.handler.RespondQueryTaskCompleted(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().RespondQueryTaskCompleted(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().RespondQueryTaskCompleted(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestSignalWithStartWorkflowExecution() {
	apiName := "SignalWithStartWorkflowExecution"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.SignalWithStartWorkflowExecutionRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.SignalWithStartWorkflowExecution(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().SignalWithStartWorkflowExecution(gomock.Any(), req).Return(&shared.StartWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().SignalWithStartWorkflowExecution(gomock.Any(), req).Return(&shared.StartWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestSignalWorkflowExecution() {
	apiName := "SignalWorkflowExecution"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.SignalWorkflowExecutionRequest{
		Domain: common.StringPtr(s.domainName),
	}
	err := s.handler.SignalWorkflowExecution(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().SignalWorkflowExecution(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().SignalWorkflowExecution(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestStartWorkflowExecution() {
	apiName := "StartWorkflowExecution"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.StartWorkflowExecutionRequest{
		Domain: common.StringPtr(s.domainName),
	}
	resp, err := s.handler.StartWorkflowExecution(context.Background(), req)
	s.Nil(err)
	// the resp is initialized to nil, since inner function is not called
	s.Nil(resp)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().StartWorkflowExecution(gomock.Any(), req).Return(&shared.StartWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().StartWorkflowExecution(gomock.Any(), req).Return(&shared.StartWorkflowExecutionResponse{}, nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

func (s *dcRedirectionHandlerSuite) TestTerminateWorkflowExecution() {
	apiName := "TerminateWorkflowExecution"

	s.mockDCRedirectionPolicy.On("WithDomainNameRedirect",
		s.domainName, apiName, mock.Anything).Return(nil).Times(1)

	req := &shared.TerminateWorkflowExecutionRequest{
		Domain: common.StringPtr(s.domainName),
	}
	err := s.handler.TerminateWorkflowExecution(context.Background(), req)
	s.Nil(err)

	callFn := s.mockDCRedirectionPolicy.Calls[0].Arguments[2].(func(string) error)
	s.mockFrontendHandler.EXPECT().TerminateWorkflowExecution(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.currentClusterName)
	s.Nil(err)
	s.mockRemoteFrontendClient.EXPECT().TerminateWorkflowExecution(gomock.Any(), req).Return(nil).Times(1)
	err = callFn(s.alternativeClusterName)
	s.Nil(err)
}

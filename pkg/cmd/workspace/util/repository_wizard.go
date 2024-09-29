// Copyright 2024 Daytona Platforms Inc.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"context"
	"fmt"

	config_const "github.com/daytonaio/daytona/cmd/daytona/config"
	apiclient_util "github.com/daytonaio/daytona/internal/util/apiclient"
	"github.com/daytonaio/daytona/pkg/apiclient"
	"github.com/daytonaio/daytona/pkg/common"
	gitprovider_view "github.com/daytonaio/daytona/pkg/views/gitprovider"
	views_util "github.com/daytonaio/daytona/pkg/views/util"
	"github.com/daytonaio/daytona/pkg/views/workspace/create"
	"github.com/daytonaio/daytona/pkg/views/workspace/selection"

	log "github.com/sirupsen/logrus"
)

func isGitProviderWithUnsupportedPagination(providerId string) bool {
	switch providerId {
	case "azure-devops", "bitbucket", "gitness", "aws-codecommit":
		return true
	default:
		return false
	}
}

type RepositoryWizardConfig struct {
	ApiClient           *apiclient.APIClient
	UserGitProviders    []apiclient.GitProvider
	Manual              bool
	MultiProject        bool
	SkipBranchSelection bool
	ProjectOrder        int
	SelectedRepos       map[string]int
}

func getRepositoryFromWizard(config RepositoryWizardConfig) (*apiclient.GitRepository, error) {
	var gitProviderConfigId string
	var namespaceId string
	var err error

	ctx := context.Background()

	samples, res, err := config.ApiClient.SampleAPI.ListSamples(ctx).Execute()
	if err != nil {
		log.Debug("Error fetching samples: ", apiclient_util.HandleErrorResponse(res, err))
	}

	if (len(config.UserGitProviders) == 0 && len(samples) == 0) || config.Manual {
		repo, err := create.GetRepositoryFromUrlInput(config.MultiProject, config.ProjectOrder, config.ApiClient, config.SelectedRepos)
		return repo, err
	}

	supportedProviders := config_const.GetSupportedGitProviders()
	var gitProviderViewList []gitprovider_view.GitProviderView

	for _, gitProvider := range config.UserGitProviders {
		for _, supportedProvider := range supportedProviders {
			if gitProvider.ProviderId == supportedProvider.Id {
				gitProviderViewList = append(gitProviderViewList,
					gitprovider_view.GitProviderView{
						Id:         gitProvider.Id,
						ProviderId: gitProvider.ProviderId,
						Name:       supportedProvider.Name,
						Username:   gitProvider.Username,
						Alias:      gitProvider.Alias,
					},
				)
			}
		}
	}

	gitProviderConfigId = selection.GetProviderIdFromPrompt(gitProviderViewList, config.ProjectOrder, len(samples) > 0)
	if gitProviderConfigId == "" {
		return nil, common.ErrCtrlCAbort
	}

	if gitProviderConfigId == selection.CustomRepoIdentifier {
		repo, err := create.GetRepositoryFromUrlInput(config.MultiProject, config.ProjectOrder, config.ApiClient, config.SelectedRepos)
		return repo, err
	}

	if gitProviderConfigId == selection.CREATE_FROM_SAMPLE {
		sample := selection.GetSampleFromPrompt(samples)
		if sample == nil {
			return nil, common.ErrCtrlCAbort
		}

		repo, res, err := config.ApiClient.GitProviderAPI.GetGitContext(ctx).Repository(apiclient.GetRepositoryContext{
			Url: sample.GitUrl,
		}).Execute()
		if err != nil {
			return nil, apiclient_util.HandleErrorResponse(res, err)
		}

		return repo, nil
	}

	var providerId string
	for _, gp := range gitProviderViewList {
		if gp.Id == gitProviderConfigId {
			providerId = gp.ProviderId
		}
	}

	var navigate string
	page := int32(1)
	perPage := int32(100)
	var namespaceList []apiclient.GitNamespace
	namespace := ""
	isOnlySingleNamespaceAvailable := true

	for {
		namespaceList = nil
		err = views_util.WithSpinner("Loading Namespaces", func() error {
			namespaces, _, err := config.ApiClient.GitProviderAPI.GetNamespaces(ctx, providerId).Page(page).PerPage(perPage).Execute()
			if err != nil {
				return err
			}
			namespaceList = append(namespaceList, namespaces...)
			return nil
		})

		if err != nil {
			return nil, err
		}

		if isOnlySingleNamespaceAvailable && len(namespaceList) == 1 {
			namespaceId = namespaceList[0].Id
			namespace = namespaceList[0].Name
			break
		} else {
			isOnlySingleNamespaceAvailable = false
		}

		// Check if the git provider supports pagination
		isPaginationDisabled := isGitProviderWithUnsupportedPagination(providerId)

		namespaceId, navigate = selection.GetNamespaceIdFromPrompt(namespaceList, config.ProjectOrder, providerId, isPaginationDisabled, page, perPage)
		if !isPaginationDisabled && navigate != "" {
			if navigate == "next" {
				page++
				continue
			} else if navigate == "prev" && page > 1 {
				page--
				continue
			}
		} else if namespaceId != "" {
			for _, namespaceItem := range namespaceList {
				if namespaceItem.Id == namespaceId {
					namespace = namespaceItem.Name
				}
			}
			break
		} else {
			// If user aborts or there's no selection
			return nil, common.ErrCtrlCAbort
		}
		for _, namespaceItem := range namespaceList {
			if namespaceItem.Id == namespaceId {
				namespace = namespaceItem.Name
			}
		}
	}

	var providerRepos []apiclient.GitRepository
	var chosenRepo *apiclient.GitRepository
	page = 1
	perPage = 100

	parentIdentifier := fmt.Sprintf("%s/%s", providerId, namespace)
	for {
		// Fetch repos for the current page
		providerRepos = nil
		err = views_util.WithSpinner("Loading Repositories", func() error {

			repos, _, err := config.ApiClient.GitProviderAPI.GetRepositories(ctx, providerId, namespaceId).Page(page).PerPage(perPage).Execute()
			if err != nil {
				return err
			}
			providerRepos = append(providerRepos, repos...)
			return nil
		})

		if err != nil {
			return nil, err
		}

		// Check if the git provider supports pagination
		// For bitbucket, pagination is only supported for GET repos api, Not for its' GET branches/ namespaces/ PRs/ branches apis.
		isPaginationDisabled := isGitProviderWithUnsupportedPagination(providerId) && providerId != "bitbucket"

		// User will either choose a repo or navigate the pages
		chosenRepo, navigate = selection.GetRepositoryFromPrompt(providerRepos, config.ProjectOrder, config.SelectedRepos, parentIdentifier, isPaginationDisabled, page, perPage)
		if !isPaginationDisabled && navigate != "" {
			if navigate == "next" {
				page++
				continue // Fetch the next page of repos
			} else if navigate == "prev" && page > 1 {
				page--
				continue // Fetch the previous page of repos
			}
		} else if chosenRepo != nil {
			break
		} else {
			// If user aborts or there's no selection
			return nil, common.ErrCtrlCAbort
		}
	}

	if config.SkipBranchSelection {
		return chosenRepo, nil
	}

	return SetBranchFromWizard(BranchWizardConfig{
		ApiClient:           config.ApiClient,
		GitProviderConfigId: gitProviderConfigId,
		NamespaceId:         namespaceId,
		Namespace:           namespace,
		ChosenRepo:          chosenRepo,
		ProjectOrder:        config.ProjectOrder,
		ProviderId:          providerId,
	})
}

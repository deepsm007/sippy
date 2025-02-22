package jiraloader

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"

	v1jira "github.com/openshift/sippy/pkg/apis/jira/v1"
	"github.com/openshift/sippy/pkg/db"
	"github.com/openshift/sippy/pkg/db/models"
	"github.com/openshift/sippy/pkg/util/sets"
)

type JIRALoader struct {
	dbc *db.DB
}

func New(dbc *db.DB) *JIRALoader {
	return &JIRALoader{
		dbc: dbc,
	}
}

const jiraTimeLayout = "2006-01-02T15:04:05.000Z0700"

func (jl *JIRALoader) LoadJIRAIncidents() error {
	start := time.Now()
	log.Infof("populating unresolved jira incident cache...")
	var dbIssues []string
	jl.dbc.DB.Table("jira_incidents").Where("resolution_time IS NULL").Pluck("key", &dbIssues)
	// unseenUnresolvedIssues contains the set of unresolved issues we have in the DB, but didn't see yet from the jira API. At the end,
	// we'll query to see what happened to the unseen issues. Most likely, we removed the trt-incident label, so we need
	// to dig into the changelog and find that state transition and consider the incident closed then.
	unseenUnresolvedIssues := sets.NewString(dbIssues...)
	log.Infof("cache populated in %+v with %d records", time.Since(start), len(dbIssues))

	start = time.Now()
	log.Infof("fetching incidents from jira...")

	/* Note a token isn't currently required to hit the issues.redhat.com API. This gets
	   us public jira cards which is probably what we want, I don't think we're ever doing non-public
	   incidents. That way we don't leak anything embargoed. If at some point we do need a token, you
	   can do it with the below, but make sure it has limited privileges and can only see public cards.

		token := os.Getenv("JIRA_TOKEN")
		req.Header.Add("Authorization", "Bearer "+token)
	*/

	client := &http.Client{}
	req, err := http.NewRequest("GET", "https://issues.redhat.com/rest/api/2/search?jql=labels%20%3D%20%22trt-incident%22%20AND%20updated%20%3E%3D%20-60d&expand=changelog", nil)
	if err != nil {
		return err
	}

	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var issues struct {
		Issues []v1jira.Issue `json:"issues"`
	}
	err = json.Unmarshal(body, &issues)
	if err != nil {
		return err
	}

	for i, issue := range issues.Issues {
		unseenUnresolvedIssues.Delete(issue.Key)

		model, err := issueToDB(&issues.Issues[i])
		if err != nil {
			log.WithError(err).Errorf("couldn't convert jira issue to db model")
			continue
		}
		if res := jl.dbc.DB.Save(model); res.Error != nil {
			log.WithError(err).Errorf("couldn't save jira incident to DB")
			return res.Error
		}
	}

	log.Infof("we have %d unseen and unresolved jira incidents", unseenUnresolvedIssues.Len())
	for _, unseen := range unseenUnresolvedIssues.List() {
		log.Infof("processing unseen, unresolved jira incidents (trt-incident label removed?)...")
		issue, err := queryJiraAPI(unseen)
		if err != nil {
			log.WithError(err).Errorf("couldn't query details for %+v", issue)
			continue
		}

		model, err := issueToDB(issue)
		if err != nil {
			log.WithError(err).Errorf("couldn't convert jira issue to db model")
			continue
		}
		if res := jl.dbc.DB.Save(model); res.Error != nil {
			log.WithError(err).Errorf("couldn't save jira incident to DB")
			return res.Error
		}
	}

	log.Infof("jira incident fetch complete in %+v", time.Since(start))
	return nil
}

func findResolutionTime(issue *v1jira.Issue) *time.Time {
	var resolutionTime *time.Time

	changelogLayout := "2006-01-02T15:04:05.999-0700"

	for _, history := range issue.Changelog.Histories {
		for _, item := range history.Items {
			// If trt-incident label was removed from a jira ticket, consider that the time when the incident
			// was resolved.
			if !issueContainsLabel(issue, "trt-incident") && item.Field == "labels" && item.FromString == "trt-incident" && item.ToString != "trt-incident" {
				createdTime, err := time.Parse(changelogLayout, history.Created)
				if err != nil {
					log.WithError(err).Warningf("parsing error: %s", history.Created)
					continue
				}
				// We pick the oldest time we removed the trt-incident label (maybe we toggled back and forth a few
				// times).
				if resolutionTime == nil || resolutionTime.Before(createdTime) {
					log.Debugf("trt-incident label was removed from %s at %+v", issue.Key, createdTime)
					resolutionTime = &createdTime
				}
			}

			// OCPBUGS don't get a resolution time until it's closed which happens when a release GA's. We
			// find the first terminal incident status changelog instead. From TRT's perspective, we don't
			// care about OCPBUGS incidents after they go to MODIFIED.
			resolvedStatuses := []string{"MODIFIED", "ON_QA", "Verified", "Closed"}
			for _, status := range resolvedStatuses {
				if item.ToString == status {
					createdTime, err := time.Parse(changelogLayout, history.Created)
					if err != nil {
						log.WithError(err).Warningf("parsing error: %s", history.Created)
						continue
					}
					// We pick the oldest state change
					if resolutionTime == nil || resolutionTime.After(createdTime) {
						log.Debugf("%s to %s at %+v", issue.Key, status, createdTime)
						resolutionTime = &createdTime
					}
				}
			}
		}
	}

	// Fallback to the jira resolution time
	if issue.Fields.ResolutionDate != "" && resolutionTime == nil {
		jiraResolutionTime, err := time.Parse(jiraTimeLayout, issue.Fields.ResolutionDate)
		if err != nil {
			fmt.Printf("parsing error: %+v", err)
		}
		log.Debugf("resolution time for %s is %+v", issue.Key, jiraResolutionTime)
		resolutionTime = &jiraResolutionTime
	}

	return resolutionTime
}

func issueContainsLabel(issue *v1jira.Issue, label string) bool {
	for _, issueLabel := range issue.Fields.Labels {
		if issueLabel == label {
			return true
		}
	}

	return false
}

// queryJiraAPI returns a singular jira issue
func queryJiraAPI(issueID string) (*v1jira.Issue, error) {
	urlFmtStr := "https://issues.redhat.com/rest/api/2/issue/%s?expand=changelog"
	client := &http.Client{}
	req, err := http.NewRequest("GET", fmt.Sprintf(urlFmtStr, issueID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("received %s from Jira API", resp.Status)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var issueDetails v1jira.Issue
	err = json.Unmarshal(bodyBytes, &issueDetails)
	if err != nil {
		return nil, err
	}

	return &issueDetails, nil
}

func issueToDB(issue *v1jira.Issue) (*models.JiraIncident, error) {
	jiraID, err := strconv.ParseUint(issue.ID, 10, 64)
	if err != nil {
		return nil, err
	}

	var startTimeP *time.Time
	if issue.Fields.Created != "" {
		startTime, err := time.Parse(jiraTimeLayout, issue.Fields.Created)
		if err != nil {
			return nil, err
		}
		startTimeP = &startTime
	}

	return &models.JiraIncident{
		Model: models.Model{
			ID: uint(jiraID),
		},
		Key:            issue.Key,
		Summary:        issue.Fields.Summary,
		StartTime:      startTimeP,
		ResolutionTime: findResolutionTime(issue),
	}, nil
}

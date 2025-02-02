package aws

import (
	"github.com/tnn-gruntwork-io/cloud-nuke/telemetry"
	commonTelemetry "github.com/tnn-gruntwork-io/go-commons/telemetry"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/tnn-gruntwork-io/cloud-nuke/config"
	"github.com/tnn-gruntwork-io/cloud-nuke/logging"
	"github.com/tnn-gruntwork-io/cloud-nuke/report"
	"github.com/tnn-gruntwork-io/go-commons/errors"
	"github.com/hashicorp/go-multierror"
)

// List all IAM Roles in the AWS account
func getAllIamRoles(session *session.Session, excludeAfter time.Time, configObj config.Config) ([]*string, error) {
	svc := iam.New(session)

	allIAMRoles := []*string{}
	err := svc.ListRolesPages(
		&iam.ListRolesInput{},
		func(page *iam.ListRolesOutput, lastPage bool) bool {
			for _, iamRole := range page.Roles {
				if shouldIncludeIAMRole(iamRole, excludeAfter, configObj) {
					allIAMRoles = append(allIAMRoles, iamRole.RoleName)
				}
			}
			return !lastPage
		},
	)
	if err != nil {
		return nil, errors.WithStackTrace(err)
	}
	return allIAMRoles, nil
}

func deleteManagedRolePolicies(svc *iam.IAM, roleName *string) error {
	policiesOutput, err := svc.ListAttachedRolePolicies(&iam.ListAttachedRolePoliciesInput{
		RoleName: roleName,
	})
	if err != nil {
		return errors.WithStackTrace(err)
	}

	for _, attachedPolicy := range policiesOutput.AttachedPolicies {
		arn := attachedPolicy.PolicyArn
		_, err = svc.DetachRolePolicy(&iam.DetachRolePolicyInput{
			PolicyArn: arn,
			RoleName:  roleName,
		})
		if err != nil {
			logging.Logger.Errorf("[Failed] %s", err)
			return errors.WithStackTrace(err)
		}
		logging.Logger.Debugf("Detached Policy %s from Role %s", aws.StringValue(arn), aws.StringValue(roleName))
	}

	return nil
}

func deleteInlineRolePolicies(svc *iam.IAM, roleName *string) error {
	policyOutput, err := svc.ListRolePolicies(&iam.ListRolePoliciesInput{
		RoleName: roleName,
	})
	if err != nil {
		logging.Logger.Debugf("[Failed] %s", err)
		return errors.WithStackTrace(err)
	}

	for _, policyName := range policyOutput.PolicyNames {
		_, err := svc.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
			PolicyName: policyName,
			RoleName:   roleName,
		})
		if err != nil {
			logging.Logger.Debugf("[Failed] %s", err)
			return errors.WithStackTrace(err)
		}
		logging.Logger.Debugf("Deleted Inline Policy %s from Role %s", aws.StringValue(policyName), aws.StringValue(roleName))
	}

	return nil
}

func deleteInstanceProfilesFromRole(svc *iam.IAM, roleName *string) error {
	profilesOutput, err := svc.ListInstanceProfilesForRole(&iam.ListInstanceProfilesForRoleInput{
		RoleName: roleName,
	})
	if err != nil {
		return errors.WithStackTrace(err)
	}

	for _, profile := range profilesOutput.InstanceProfiles {

		// Role needs to be removed from instance profile before it can be deleted
		_, err := svc.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
			InstanceProfileName: profile.InstanceProfileName,
			RoleName:            roleName,
		})
		if err != nil {
			logging.Logger.Debugf("[Failed] %s", err)
			return errors.WithStackTrace(err)
		} else {
			_, err := svc.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
				InstanceProfileName: profile.InstanceProfileName,
			})
			if err != nil {
				logging.Logger.Errorf("[Failed] %s", err)
				return errors.WithStackTrace(err)
			}
		}
		logging.Logger.Debugf("Detached and Deleted InstanceProfile %s from Role %s", aws.StringValue(profile.InstanceProfileName), aws.StringValue(roleName))
	}
	return nil
}

func deleteIamRole(svc *iam.IAM, roleName *string) error {
	_, err := svc.DeleteRole(&iam.DeleteRoleInput{
		RoleName: roleName,
	})
	if err != nil {
		return errors.WithStackTrace(err)
	}

	return nil
}

// Delete all IAM Roles
func nukeAllIamRoles(session *session.Session, roleNames []*string) error {
	region := aws.StringValue(session.Config.Region)
	svc := iam.New(session)

	if len(roleNames) == 0 {
		logging.Logger.Debug("No IAM Roles to nuke")
		return nil
	}

	// NOTE: we don't need to do pagination here, because the pagination is handled by the caller to this function,
	// based on IAMRoles.MaxBatchSize, however we add a guard here to warn users when the batching fails and has a
	// chance of throttling AWS. Since we concurrently make one call for each identifier, we pick 100 for the limit here
	// because many APIs in AWS have a limit of 100 requests per second.
	if len(roleNames) > 100 {
		logging.Logger.Debugf("Nuking too many IAM Roles at once (100): halting to avoid hitting AWS API rate limiting")
		return TooManyIamRoleErr{}
	}

	// There is no bulk delete IAM Roles API, so we delete the batch of IAM roles concurrently using go routines
	logging.Logger.Debugf("Deleting all IAM Roles in region %s", region)
	wg := new(sync.WaitGroup)
	wg.Add(len(roleNames))
	errChans := make([]chan error, len(roleNames))
	for i, roleName := range roleNames {
		errChans[i] = make(chan error, 1)
		go deleteIamRoleAsync(wg, errChans[i], svc, roleName)
	}
	wg.Wait()

	// Collect all the errors from the async delete calls into a single error struct.
	var allErrs *multierror.Error
	for _, errChan := range errChans {
		if err := <-errChan; err != nil {
			allErrs = multierror.Append(allErrs, err)
			logging.Logger.Debugf("[Failed] %s", err)
			telemetry.TrackEvent(commonTelemetry.EventContext{
				EventName: "Error Nuking IAM Role",
			}, map[string]interface{}{
				"region": *session.Config.Region,
			})
		}
	}
	finalErr := allErrs.ErrorOrNil()
	if finalErr != nil {
		return errors.WithStackTrace(finalErr)
	}

	for _, roleName := range roleNames {
		logging.Logger.Debugf("[OK] IAM Role %s was deleted in %s", aws.StringValue(roleName), region)
	}
	return nil
}

func shouldIncludeIAMRole(iamRole *iam.Role, excludeAfter time.Time, configObj config.Config) bool {
	if iamRole == nil {
		return false
	}

	// The OrganizationAccountAccessRole is a special role that is created by AWS Organizations, and is used to allow
	// users to access the AWS account. We should not delete this role, so we can filter it out of the Roles found and
	// managed by cloud-nuke.
	if strings.Contains(aws.StringValue(iamRole.RoleName), "OrganizationAccountAccessRole") {
		return false
	}

	// The ARNs of AWS-reserved IAM roles, which can only be modified or deleted by AWS, contain "aws-reserved", so we can filter them out
	// of the Roles found and managed by cloud-nuke
	if strings.Contains(aws.StringValue(iamRole.Arn), "aws-reserved") {
		return false
	}

	if excludeAfter.Before(*iamRole.CreateDate) {
		return false
	}

	return config.ShouldInclude(
		aws.StringValue(iamRole.RoleName),
		configObj.IAMRoles.IncludeRule.NamesRegExp,
		configObj.IAMRoles.ExcludeRule.NamesRegExp,
	)
}

func deleteIamRoleAsync(wg *sync.WaitGroup, errChan chan error, svc *iam.IAM, roleName *string) {
	defer wg.Done()

	var result *multierror.Error

	// Functions used to really nuke an IAM Role as a role can have many attached
	// items we need delete/detach them before actually deleting it.
	// NOTE: The actual role deletion should always be the last one. This way we
	// can guarantee that it will fail if we forgot to delete/detach an item.
	functions := []func(svc *iam.IAM, roleName *string) error{
		deleteInstanceProfilesFromRole,
		deleteInlineRolePolicies,
		deleteManagedRolePolicies,
		deleteIamRole,
	}

	for _, fn := range functions {
		if err := fn(svc, roleName); err != nil {
			result = multierror.Append(result, err)
		}
	}

	// Record status of this resource
	e := report.Entry{
		Identifier:   aws.StringValue(roleName),
		ResourceType: "IAM Role",
		Error:        result.ErrorOrNil(),
	}
	report.Record(e)

	errChan <- result.ErrorOrNil()
}

// Custom errors

type TooManyIamRoleErr struct{}

func (err TooManyIamRoleErr) Error() string {
	return "Too many IAM Roles requested at once"
}

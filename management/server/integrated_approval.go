package server

import (
	"errors"

	"github.com/google/martian/v3/log"

	"github.com/netbirdio/netbird/management/server/account"
	nbpeer "github.com/netbirdio/netbird/management/server/peer"
)

// UpdateIntegratedApprovalGroups updates the integrated approval groups for a specified account.
// It retrieves the account associated with the provided userID, then updates the integrated approval groups
// with the provided list of group ids. The updated account is then saved.
//
// Parameters:
//   - accountID: The ID of the account for which integrated approval groups are to be updated.
//   - userID: The ID of the user whose account is being updated.
//   - groups: A slice of strings representing the ids of integrated approval groups to be updated.
//
// Returns:
//   - error: An error if any occurred during the process, otherwise returns nil
func (am *DefaultAccountManager) UpdateIntegratedApprovalGroups(accountID string, userID string, groups []string) error {
	ok, err := am.GroupValidation(accountID, groups)
	if err != nil {
		log.Debugf("error validating groups: %s", err.Error())
		return err
	}

	if !ok {
		log.Debugf("invalid groups")
		return errors.New("invalid groups")
	}

	unlock := am.Store.AcquireAccountLock(accountID)
	defer unlock()

	a, err := am.Store.GetAccountByUser(userID)
	if err != nil {
		return err
	}

	var extra *account.ExtraSettings

	if a.Settings.Extra != nil {
		extra = a.Settings.Extra
	} else {
		extra = &account.ExtraSettings{}
		a.Settings.Extra = extra
	}
	extra.IntegratedApprovalGroups = groups
	return am.Store.SaveAccount(a)
}

func (am *DefaultAccountManager) IsPeerRequiresApproval(accountID string, peer *nbpeer.Peer) bool {
	return am.integratedPeerValidator.IsRequiresApproval(accountID, peer, nil, nil)
}

func (am *DefaultAccountManager) GroupValidation(accountId string, groups []string) (bool, error) {
	if len(groups) == 0 {
		return true, nil
	}
	accountsGroups, err := am.ListGroups(accountId)
	if err != nil {
		return false, err
	}
	for _, group := range groups {
		var found bool
		for _, accountGroup := range accountsGroups {
			if accountGroup.ID == group {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}

	return true, nil
}

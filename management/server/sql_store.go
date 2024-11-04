package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	nbdns "github.com/netbirdio/netbird/dns"
	"github.com/netbirdio/netbird/management/server/account"
	nbgroup "github.com/netbirdio/netbird/management/server/group"
	nbpeer "github.com/netbirdio/netbird/management/server/peer"
	"github.com/netbirdio/netbird/management/server/posture"
	"github.com/netbirdio/netbird/management/server/status"
	"github.com/netbirdio/netbird/management/server/telemetry"
	"github.com/netbirdio/netbird/route"
)

const (
	storeSqliteFileName        = "store.db"
	idQueryCondition           = "id = ?"
	keyQueryCondition          = "key = ?"
	accountAndIDQueryCondition = "account_id = ? and id = ?"
	accountIDCondition         = "account_id = ?"
	peerNotFoundFMT            = "peer %s not found"
	batchSize                  = 500
)

// SqlStore represents an account storage backed by a Sql DB persisted to disk
type SqlStore struct {
	db                *gorm.DB
	resourceLocks     sync.Map
	globalAccountLock sync.Mutex
	metrics           telemetry.AppMetrics
	installationPK    int
	storeEngine       StoreEngine
}

type installation struct {
	ID                  uint `gorm:"primaryKey"`
	InstallationIDValue string
}

type migrationFunc func(*gorm.DB) error

// NewSqlStore creates a new SqlStore instance.
func NewSqlStore(ctx context.Context, db *gorm.DB, storeEngine StoreEngine, metrics telemetry.AppMetrics) (*SqlStore, error) {
	sql, err := db.DB()
	if err != nil {
		return nil, err
	}

	conns, err := strconv.Atoi(os.Getenv("NB_SQL_MAX_OPEN_CONNS"))
	if err != nil {
		conns = runtime.NumCPU()
	}
	sql.SetMaxOpenConns(conns)

	log.Infof("Set max open db connections to %d", conns)

	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	err = db.AutoMigrate(
		&SetupKey{}, &nbpeer.Peer{}, &User{}, &PersonalAccessToken{}, &nbgroup.Group{},
		&Account{}, &Policy{}, &PolicyRule{}, &route.Route{}, &nbdns.NameServerGroup{},
		&installation{}, &account.ExtraSettings{}, &posture.Checks{}, &nbpeer.NetworkAddress{},
	)
	if err != nil {
		return nil, fmt.Errorf("auto migrate: %w", err)
	}

	return &SqlStore{db: db, storeEngine: storeEngine, metrics: metrics, installationPK: 1}, nil
}

// AcquireGlobalLock acquires global lock across all the accounts and returns a function that releases the lock
func (s *SqlStore) AcquireGlobalLock(ctx context.Context) (unlock func()) {
	log.WithContext(ctx).Tracef("acquiring global lock")
	start := time.Now()
	s.globalAccountLock.Lock()

	unlock = func() {
		s.globalAccountLock.Unlock()
		log.WithContext(ctx).Tracef("released global lock in %v", time.Since(start))
	}

	took := time.Since(start)
	log.WithContext(ctx).Tracef("took %v to acquire global lock", took)
	if s.metrics != nil {
		s.metrics.StoreMetrics().CountGlobalLockAcquisitionDuration(took)
	}

	return unlock
}

// AcquireWriteLockByUID acquires an ID lock for writing to a resource and returns a function that releases the lock
func (s *SqlStore) AcquireWriteLockByUID(ctx context.Context, uniqueID string) (unlock func()) {
	log.WithContext(ctx).Tracef("acquiring write lock for ID %s", uniqueID)

	start := time.Now()
	value, _ := s.resourceLocks.LoadOrStore(uniqueID, &sync.RWMutex{})
	mtx := value.(*sync.RWMutex)
	mtx.Lock()

	unlock = func() {
		mtx.Unlock()
		log.WithContext(ctx).Tracef("released write lock for ID %s in %v", uniqueID, time.Since(start))
	}

	return unlock
}

// AcquireReadLockByUID acquires an ID lock for writing to a resource and returns a function that releases the lock
func (s *SqlStore) AcquireReadLockByUID(ctx context.Context, uniqueID string) (unlock func()) {
	log.WithContext(ctx).Tracef("acquiring read lock for ID %s", uniqueID)

	start := time.Now()
	value, _ := s.resourceLocks.LoadOrStore(uniqueID, &sync.RWMutex{})
	mtx := value.(*sync.RWMutex)
	mtx.RLock()

	unlock = func() {
		mtx.RUnlock()
		log.WithContext(ctx).Tracef("released read lock for ID %s in %v", uniqueID, time.Since(start))
	}

	return unlock
}

func (s *SqlStore) CreateAccount(ctx context.Context, lockStrength LockingStrength, account *Account) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Create(&account)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save new account in store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to save new account in store")
	}
	return nil
}

func (s *SqlStore) SaveAccount(ctx context.Context, account *Account) error {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		if elapsed > 1*time.Second {
			log.WithContext(ctx).Tracef("SaveAccount for account %s exceeded 1s, took: %v", account.Id, elapsed)
		}
	}()

	// todo: remove this check after the issue is resolved
	s.checkAccountDomainBeforeSave(ctx, account.Id, account.Domain)

	generateAccountSQLTypes(account)

	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Select(clause.Associations).Delete(account.Policies, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account.UsersG, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account)
		if result.Error != nil {
			return result.Error
		}

		result = tx.
			Session(&gorm.Session{FullSaveAssociations: true}).
			Clauses(clause.OnConflict{UpdateAll: true}).
			Create(account)
		if result.Error != nil {
			return result.Error
		}
		return nil
	})

	took := time.Since(start)
	if s.metrics != nil {
		s.metrics.StoreMetrics().CountPersistenceDuration(took)
	}
	log.WithContext(ctx).Debugf("took %d ms to persist an account to the store", took.Milliseconds())

	return err
}

// generateAccountSQLTypes generates the GORM compatible types for the account
func generateAccountSQLTypes(account *Account) {
	for _, key := range account.SetupKeys {
		account.SetupKeysG = append(account.SetupKeysG, *key)
	}

	for id, peer := range account.Peers {
		peer.ID = id
		account.PeersG = append(account.PeersG, *peer)
	}

	for id, user := range account.Users {
		user.Id = id
		for id, pat := range user.PATs {
			pat.ID = id
			user.PATsG = append(user.PATsG, *pat)
		}
		account.UsersG = append(account.UsersG, *user)
	}

	for id, group := range account.Groups {
		group.ID = id
		account.GroupsG = append(account.GroupsG, *group)
	}

	for id, route := range account.Routes {
		route.ID = id
		account.RoutesG = append(account.RoutesG, *route)
	}

	for id, ns := range account.NameServerGroups {
		ns.ID = id
		account.NameServerGroupsG = append(account.NameServerGroupsG, *ns)
	}
}

// checkAccountDomainBeforeSave temporary method to troubleshoot an issue with domains getting blank
func (s *SqlStore) checkAccountDomainBeforeSave(ctx context.Context, accountID, newDomain string) {
	var acc Account
	var domain string
	result := s.db.Model(&acc).Select("domain").Where(idQueryCondition, accountID).First(&domain)
	if result.Error != nil {
		if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			log.WithContext(ctx).Errorf("error when getting account %s from the store to check domain: %s", accountID, result.Error)
		}
		return
	}
	if domain != "" && newDomain == "" {
		log.WithContext(ctx).Warnf("saving an account with empty domain when there was a domain set. Previous domain %s, Account ID: %s, Trace: %s", domain, accountID, debug.Stack())
	}
}

func (s *SqlStore) DeleteAccount(ctx context.Context, account *Account) error {
	start := time.Now()

	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Select(clause.Associations).Delete(account.Policies, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account.UsersG, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account)
		if result.Error != nil {
			return result.Error
		}

		return nil
	})

	took := time.Since(start)
	if s.metrics != nil {
		s.metrics.StoreMetrics().CountPersistenceDuration(took)
	}
	log.WithContext(ctx).Debugf("took %d ms to delete an account to the store", took.Milliseconds())

	return err
}

func (s *SqlStore) SaveInstallationID(_ context.Context, ID string) error {
	installation := installation{InstallationIDValue: ID}
	installation.ID = uint(s.installationPK)

	return s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&installation).Error
}

func (s *SqlStore) GetInstallationID() string {
	var installation installation

	if result := s.db.First(&installation, idQueryCondition, s.installationPK); result.Error != nil {
		return ""
	}

	return installation.InstallationIDValue
}

func (s *SqlStore) SavePeer(ctx context.Context, lockStrength LockingStrength, accountID string, peer *nbpeer.Peer) error {
	// To maintain data integrity, we create a copy of the peer's to prevent unintended updates to other fields.
	peerCopy := peer.Copy()
	peerCopy.AccountID = accountID

	err := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Transaction(func(tx *gorm.DB) error {
		// check if peer exists before saving
		var peerID string
		result := tx.Model(&nbpeer.Peer{}).Select("id").Find(&peerID, accountAndIDQueryCondition, accountID, peer.ID)
		if result.Error != nil {
			return result.Error
		}

		if peerID == "" {
			return status.Errorf(status.NotFound, peerNotFoundFMT, peer.ID)
		}

		result = tx.Model(&nbpeer.Peer{}).Where(accountAndIDQueryCondition, accountID, peer.ID).Save(peerCopy)
		if result.Error != nil {
			return result.Error
		}

		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

func (s *SqlStore) UpdateAccountDomainAttributes(ctx context.Context, lockStrength LockingStrength, accountID string, domain string, category string, isPrimaryDomain *bool) error {
	accountCopy := Account{
		Domain:         domain,
		DomainCategory: category,
	}

	fieldsToUpdate := []string{"domain", "domain_category"}
	if isPrimaryDomain != nil {
		accountCopy.IsDomainPrimaryAccount = *isPrimaryDomain
		fieldsToUpdate = append(fieldsToUpdate, "is_domain_primary_account")
	}

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		Select(fieldsToUpdate).
		Where(idQueryCondition, accountID).
		Updates(&accountCopy)
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "account %s", accountID)
	}

	return nil
}

func (s *SqlStore) SavePeerStatus(ctx context.Context, lockStrength LockingStrength, accountID, peerID string, peerStatus nbpeer.PeerStatus) error {
	var peerCopy nbpeer.Peer
	peerCopy.Status = &peerStatus

	fieldsToUpdate := []string{
		"peer_status_last_seen", "peer_status_connected",
		"peer_status_login_expired", "peer_status_required_approval",
	}
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&nbpeer.Peer{}).
		Select(fieldsToUpdate).
		Where(accountAndIDQueryCondition, accountID, peerID).
		Updates(&peerCopy)
	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, peerNotFoundFMT, peerID)
	}

	return nil
}

func (s *SqlStore) SavePeerLocation(ctx context.Context, lockStrength LockingStrength, accountID string, peerWithLocation *nbpeer.Peer) error {
	// To maintain data integrity, we create a copy of the peer's location to prevent unintended updates to other fields.
	var peerCopy nbpeer.Peer
	// Since the location field has been migrated to JSON serialization,
	// updating the struct ensures the correct data format is inserted into the database.
	peerCopy.Location = peerWithLocation.Location

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&nbpeer.Peer{}).
		Where(accountAndIDQueryCondition, accountID, peerWithLocation.ID).
		Updates(peerCopy)

	if result.Error != nil {
		return result.Error
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, peerNotFoundFMT, peerWithLocation.ID)
	}

	return nil
}

// SaveUsers saves the given list of users to the database.
func (s *SqlStore) SaveUsers(ctx context.Context, lockStrength LockingStrength, users []*User) error {
	if len(users) == 0 {
		return nil
	}

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(&users)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save users to store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to save users to store")
	}
	return nil
}

// SaveUser saves the given user to the database.
func (s *SqlStore) SaveUser(ctx context.Context, lockStrength LockingStrength, user *User) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(user)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save user to store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to save user to store")
	}
	return nil
}

// SaveGroups saves the given list of groups to the database.
func (s *SqlStore) SaveGroups(ctx context.Context, lockStrength LockingStrength, groups []*nbgroup.Group) error {
	if len(groups) == 0 {
		return nil
	}

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(&groups)
	if result.Error != nil {
		return status.Errorf(status.Internal, "failed to save groups to store: %v", result.Error)
	}
	return nil
}

func (s *SqlStore) GetAccountByPrivateDomain(ctx context.Context, domain string) (*Account, error) {
	accountID, err := s.GetAccountIDByPrivateDomain(ctx, LockingStrengthShare, domain)
	if err != nil {
		return nil, err
	}

	// TODO:  rework to not call GetAccount
	return s.GetAccount(ctx, accountID)
}

func (s *SqlStore) GetAccountIDByPrivateDomain(ctx context.Context, lockStrength LockingStrength, domain string) (string, error) {
	var accountID string
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).Select("id").
		Where("domain = ? and is_domain_primary_account = ? and domain_category = ?",
			strings.ToLower(domain), true, PrivateCategory,
		).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "account not found: provided domain is not registered or is not private")
		}
		log.WithContext(ctx).Errorf("error when getting account from the store: %s", result.Error)
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountBySetupKey(ctx context.Context, setupKey string) (*Account, error) {
	var key SetupKey
	result := s.db.WithContext(ctx).Select("account_id").First(&key, keyQueryCondition, setupKey)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return nil, status.NewSetupKeyNotFoundError(result.Error)
	}

	if key.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, key.AccountID)
}

func (s *SqlStore) GetUserByUserID(ctx context.Context, lockStrength LockingStrength, userID string) (*User, error) {
	var user User
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Preload(clause.Associations).First(&user, idQueryCondition, userID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewUserNotFoundError(userID)
		}
		log.WithContext(ctx).Errorf("failed to get user from the store: %s", result.Error)
		return nil, status.NewGetUserFromStoreError()
	}

	return &user, nil
}

func (s *SqlStore) DeleteUser(ctx context.Context, lockStrength LockingStrength, accountID, userID string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&User{}, accountAndIDQueryCondition, accountID, userID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete user from the store: %s", err)
		return status.Errorf(status.Internal, "failed to user policy from store")
	}

	if result.RowsAffected == 0 {
		return status.NewUserNotFoundError(userID)
	}

	return nil
}

func (s *SqlStore) GetUserByPATID(ctx context.Context, lockStrength LockingStrength, patID string) (*User, error) {
	var user User
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Joins("JOIN personal_access_tokens ON personal_access_tokens.user_id = users.id").
		Where("personal_access_tokens.id = ?", patID).First(&user)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPATNotFoundError()
		}
		log.WithContext(ctx).Errorf("failed to get token user from the store: %s", result.Error)
		return nil, status.NewGetUserFromStoreError()
	}

	return &user, nil
}

func (s *SqlStore) GetAccountUsers(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*User, error) {
	var users []*User
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Find(&users, accountIDCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "accountID not found: index lookup failed")
		}
		log.WithContext(ctx).Errorf("error when getting users from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "issue getting users from store")
	}

	return users, nil
}

func (s *SqlStore) GetAccountGroups(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbgroup.Group, error) {
	var groups []*nbgroup.Group
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Find(&groups, accountIDCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "accountID not found: index lookup failed")
		}
		log.WithContext(ctx).Errorf("error when getting groups from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "issue getting groups from store")
	}

	return groups, nil
}

func (s *SqlStore) GetAllAccounts(ctx context.Context) (all []*Account) {
	var accounts []Account
	result := s.db.Find(&accounts)
	if result.Error != nil {
		return all
	}

	for _, account := range accounts {
		if acc, err := s.GetAccount(ctx, account.Id); err == nil {
			all = append(all, acc)
		}
	}

	return all
}

func (s *SqlStore) GetAllAccountIDs(ctx context.Context, lockStrength LockingStrength) ([]string, error) {
	var accountIDs []string
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Model(&Account{}).Pluck("id", &accountIDs)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get account IDs from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get account IDs from store")
	}

	return accountIDs, nil
}

func (s *SqlStore) GetAccount(ctx context.Context, accountID string) (*Account, error) {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		if elapsed > 1*time.Second {
			log.WithContext(ctx).Tracef("GetAccount for account %s exceeded 1s, took: %v", accountID, elapsed)
		}
	}()

	var account Account
	result := s.db.Model(&account).
		Preload("UsersG.PATsG"). // have to be specifies as this is nester reference
		Preload(clause.Associations).
		First(&account, idQueryCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("error when getting account %s from the store: %s", accountID, result.Error)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewAccountNotFoundError()
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	// we have to manually preload policy rules as it seems that gorm preloading doesn't do it for us
	for i, policy := range account.Policies {
		var rules []*PolicyRule
		err := s.db.Model(&PolicyRule{}).Find(&rules, "policy_id = ?", policy.ID).Error
		if err != nil {
			return nil, status.Errorf(status.NotFound, "rule not found")
		}
		account.Policies[i].Rules = rules
	}

	account.SetupKeys = make(map[string]*SetupKey, len(account.SetupKeysG))
	for _, key := range account.SetupKeysG {
		account.SetupKeys[key.Key] = key.Copy()
	}
	account.SetupKeysG = nil

	account.Peers = make(map[string]*nbpeer.Peer, len(account.PeersG))
	for _, peer := range account.PeersG {
		account.Peers[peer.ID] = peer.Copy()
	}
	account.PeersG = nil

	account.Users = make(map[string]*User, len(account.UsersG))
	for _, user := range account.UsersG {
		user.PATs = make(map[string]*PersonalAccessToken, len(user.PATs))
		for _, pat := range user.PATsG {
			user.PATs[pat.ID] = pat.Copy()
		}
		account.Users[user.Id] = user.Copy()
	}
	account.UsersG = nil

	account.Groups = make(map[string]*nbgroup.Group, len(account.GroupsG))
	for _, group := range account.GroupsG {
		account.Groups[group.ID] = group.Copy()
	}
	account.GroupsG = nil

	account.Routes = make(map[route.ID]*route.Route, len(account.RoutesG))
	for _, route := range account.RoutesG {
		account.Routes[route.ID] = route.Copy()
	}
	account.RoutesG = nil

	account.NameServerGroups = make(map[string]*nbdns.NameServerGroup, len(account.NameServerGroupsG))
	for _, ns := range account.NameServerGroupsG {
		account.NameServerGroups[ns.ID] = ns.Copy()
	}
	account.NameServerGroupsG = nil

	return &account, nil
}

func (s *SqlStore) GetAccountByUser(ctx context.Context, userID string) (*Account, error) {
	var user User
	result := s.db.WithContext(ctx).Select("account_id").First(&user, idQueryCondition, userID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	if user.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, user.AccountID)
}

func (s *SqlStore) GetAccountByPeerID(ctx context.Context, peerID string) (*Account, error) {
	var peer nbpeer.Peer
	result := s.db.WithContext(ctx).Select("account_id").First(&peer, idQueryCondition, peerID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	if peer.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, peer.AccountID)
}

func (s *SqlStore) GetAccountByPeerPubKey(ctx context.Context, peerKey string) (*Account, error) {
	var peer nbpeer.Peer

	result := s.db.WithContext(ctx).Select("account_id").First(&peer, keyQueryCondition, peerKey)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	if peer.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, peer.AccountID)
}

func (s *SqlStore) GetAccountIDByPeerPubKey(ctx context.Context, peerKey string) (string, error) {
	var peer nbpeer.Peer
	var accountID string
	result := s.db.WithContext(ctx).Model(&peer).Select("account_id").Where(keyQueryCondition, peerKey).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.NewAccountNotFoundError()
		}
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountIDByUserID(ctx context.Context, lockStrength LockingStrength, userID string) (string, error) {
	var accountID string
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&User{}).
		Select("account_id").Where(idQueryCondition, userID).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.NewAccountNotFoundError()
		}
		log.WithContext(ctx).Errorf("failed to get accountID from the store: %s", result.Error)
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountIDByPeerID(ctx context.Context, lockStrength LockingStrength, peerID string) (string, error) {
	var accountID string
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&nbpeer.Peer{}).
		Select("account_id").Where(idQueryCondition, peerID).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountIDBySetupKey(ctx context.Context, setupKey string) (string, error) {
	var accountID string
	result := s.db.WithContext(ctx).Model(&SetupKey{}).Select("account_id").Where(keyQueryCondition, setupKey).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return "", status.NewSetupKeyNotFoundError(result.Error)
	}

	if accountID == "" {
		return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return accountID, nil
}

func (s *SqlStore) GetTakenIPs(ctx context.Context, lockStrength LockingStrength, accountID string) ([]net.IP, error) {
	var ipJSONStrings []string

	// Fetch the IP addresses as JSON strings
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&nbpeer.Peer{}).
		Where("account_id = ?", accountID).
		Pluck("ip", &ipJSONStrings)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "no peers found for the account")
		}
		return nil, status.Errorf(status.Internal, "issue getting IPs from store: %s", result.Error)
	}

	// Convert the JSON strings to net.IP objects
	ips := make([]net.IP, len(ipJSONStrings))
	for i, ipJSON := range ipJSONStrings {
		var ip net.IP
		if err := json.Unmarshal([]byte(ipJSON), &ip); err != nil {
			return nil, status.Errorf(status.Internal, "issue parsing IP JSON from store")
		}
		ips[i] = ip
	}

	return ips, nil
}

// GetAccountPeerDNSLabels retrieves all unique DNS labels for peers associated with a specified account.
func (s *SqlStore) GetAccountPeerDNSLabels(ctx context.Context, lockStrength LockingStrength, accountID string) ([]string, error) {
	var labels []string

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&nbpeer.Peer{}).
		Where("account_id = ?", accountID).
		Pluck("dns_label", &labels)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "no peers found for the account")
		}
		log.WithContext(ctx).Errorf("error when getting dns labels from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "issue getting dns labels from store: %s", result.Error)
	}

	return labels, nil
}

func (s *SqlStore) GetAccountNetwork(ctx context.Context, lockStrength LockingStrength, accountID string) (*Network, error) {
	var accountNetwork AccountNetwork

	if err := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		First(&accountNetwork, idQueryCondition, accountID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.NewAccountNotFoundError()
		}
		log.WithContext(ctx).Errorf("error when getting network from the store: %s", err)
		return nil, status.Errorf(status.Internal, "issue getting network from store: %s", err)
	}
	return accountNetwork.Network, nil
}

func (s *SqlStore) GetPeerByPeerPubKey(ctx context.Context, lockStrength LockingStrength, peerKey string) (*nbpeer.Peer, error) {
	var peer nbpeer.Peer
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).First(&peer, keyQueryCondition, peerKey)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "peer not found")
		}
		return nil, status.Errorf(status.Internal, "issue getting peer from store: %s", result.Error)
	}

	return &peer, nil
}

func (s *SqlStore) GetAccountSettings(ctx context.Context, lockStrength LockingStrength, accountID string) (*Settings, error) {
	var accountSettings AccountSettings
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		First(&accountSettings, idQueryCondition, accountID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "settings not found")
		}
		return nil, status.Errorf(status.Internal, "issue getting settings from store: %s", err)
	}
	return accountSettings.Settings, nil
}

func (s *SqlStore) GetAccountCreatedBy(ctx context.Context, lockStrength LockingStrength, accountID string) (string, error) {
	var createdBy string
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		Select("created_by").First(&createdBy, idQueryCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.NewAccountNotFoundError()
		}
		log.WithContext(ctx).Errorf("error when getting account creator from the store: %s", result.Error)
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return createdBy, nil
}

// SaveAccountSettings stores the account settings in DB.
func (s *SqlStore) SaveAccountSettings(ctx context.Context, lockStrength LockingStrength, accountID string, settings *Settings) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		Select("*").Where(idQueryCondition, accountID).Updates(&AccountSettings{Settings: settings})
	if result.Error != nil {
		return status.Errorf(status.Internal, "failed to save account settings to store: %v", result.Error)
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "account not found")
	}

	return nil
}

// SaveUserLastLogin stores the last login time for a user in DB.
func (s *SqlStore) SaveUserLastLogin(ctx context.Context, accountID, userID string, lastLogin time.Time) error {
	var user User

	result := s.db.WithContext(ctx).First(&user, accountAndIDQueryCondition, accountID, userID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return status.NewUserNotFoundError(userID)
		}
		return status.NewGetUserFromStoreError()
	}
	user.LastLogin = lastLogin

	return s.db.Save(&user).Error
}

func (s *SqlStore) GetPostureCheckByChecksDefinition(accountID string, checks *posture.ChecksDefinition) (*posture.Checks, error) {
	definitionJSON, err := json.Marshal(checks)
	if err != nil {
		return nil, err
	}

	var postureCheck posture.Checks
	err = s.db.Where("account_id = ? AND checks = ?", accountID, string(definitionJSON)).First(&postureCheck).Error
	if err != nil {
		return nil, err
	}

	return &postureCheck, nil
}

// Close closes the underlying DB connection
func (s *SqlStore) Close(_ context.Context) error {
	sql, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("get db: %w", err)
	}
	return sql.Close()
}

// GetStoreEngine returns underlying store engine
func (s *SqlStore) GetStoreEngine() StoreEngine {
	return s.storeEngine
}

// NewSqliteStore creates a new SQLite store.
func NewSqliteStore(ctx context.Context, dataDir string, metrics telemetry.AppMetrics) (*SqlStore, error) {
	storeStr := fmt.Sprintf("%s?cache=shared", storeSqliteFileName)
	if runtime.GOOS == "windows" {
		// Vo avoid `The process cannot access the file because it is being used by another process` on Windows
		storeStr = storeSqliteFileName
	}

	file := filepath.Join(dataDir, storeStr)
	db, err := gorm.Open(sqlite.Open(file), getGormConfig())
	if err != nil {
		return nil, err
	}

	return NewSqlStore(ctx, db, SqliteStoreEngine, metrics)
}

// NewPostgresqlStore creates a new Postgres store.
func NewPostgresqlStore(ctx context.Context, dsn string, metrics telemetry.AppMetrics) (*SqlStore, error) {
	db, err := gorm.Open(postgres.Open(dsn), getGormConfig())
	if err != nil {
		return nil, err
	}

	return NewSqlStore(ctx, db, PostgresStoreEngine, metrics)
}

func getGormConfig() *gorm.Config {
	return &gorm.Config{
		Logger:          logger.Default.LogMode(logger.Silent),
		CreateBatchSize: 400,
		PrepareStmt:     true,
	}
}

// newPostgresStore initializes a new Postgres store.
func newPostgresStore(ctx context.Context, metrics telemetry.AppMetrics) (Store, error) {
	dsn, ok := os.LookupEnv(postgresDsnEnv)
	if !ok {
		return nil, fmt.Errorf("%s is not set", postgresDsnEnv)
	}
	return NewPostgresqlStore(ctx, dsn, metrics)
}

// NewSqliteStoreFromFileStore restores a store from FileStore and stores SQLite DB in the file located in datadir.
func NewSqliteStoreFromFileStore(ctx context.Context, fileStore *FileStore, dataDir string, metrics telemetry.AppMetrics) (*SqlStore, error) {
	store, err := NewSqliteStore(ctx, dataDir, metrics)
	if err != nil {
		return nil, err
	}

	err = store.SaveInstallationID(ctx, fileStore.InstallationID)
	if err != nil {
		return nil, err
	}

	for _, account := range fileStore.GetAllAccounts(ctx) {
		err := store.SaveAccount(ctx, account)
		if err != nil {
			return nil, err
		}
	}

	return store, nil
}

// NewPostgresqlStoreFromSqlStore restores a store from SqlStore and stores Postgres DB.
func NewPostgresqlStoreFromSqlStore(ctx context.Context, sqliteStore *SqlStore, dsn string, metrics telemetry.AppMetrics) (*SqlStore, error) {
	store, err := NewPostgresqlStore(ctx, dsn, metrics)
	if err != nil {
		return nil, err
	}

	err = store.SaveInstallationID(ctx, sqliteStore.GetInstallationID())
	if err != nil {
		return nil, err
	}

	for _, account := range sqliteStore.GetAllAccounts(ctx) {
		err := store.SaveAccount(ctx, account)
		if err != nil {
			return nil, err
		}
	}

	return store, nil
}

func (s *SqlStore) GetSetupKeyBySecret(ctx context.Context, lockStrength LockingStrength, key string) (*SetupKey, error) {
	var setupKey SetupKey
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&setupKey, keyQueryCondition, key)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "setup key not found")
		}
		return nil, status.NewSetupKeyNotFoundError(result.Error)
	}
	return &setupKey, nil
}

func (s *SqlStore) IncrementSetupKeyUsage(ctx context.Context, setupKeyID string) error {
	result := s.db.WithContext(ctx).Model(&SetupKey{}).
		Where(idQueryCondition, setupKeyID).
		Updates(map[string]interface{}{
			"used_times": gorm.Expr("used_times + 1"),
			"last_used":  time.Now(),
		})

	if result.Error != nil {
		return status.Errorf(status.Internal, "issue incrementing setup key usage count: %s", result.Error)
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "setup key not found")
	}

	return nil
}

func (s *SqlStore) AddPeerToAllGroup(ctx context.Context, accountID string, peerID string) error {
	var group nbgroup.Group

	result := s.db.WithContext(ctx).Where("account_id = ? AND name = ?", accountID, "All").First(&group)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return status.Errorf(status.NotFound, "group 'All' not found for account")
		}
		return status.Errorf(status.Internal, "issue finding group 'All': %s", result.Error)
	}

	for _, existingPeerID := range group.Peers {
		if existingPeerID == peerID {
			return nil
		}
	}

	group.Peers = append(group.Peers, peerID)

	if err := s.db.Save(&group).Error; err != nil {
		return status.Errorf(status.Internal, "issue updating group 'All': %s", err)
	}

	return nil
}

func (s *SqlStore) AddPeerToGroup(ctx context.Context, accountId string, peerId string, groupID string) error {
	var group nbgroup.Group

	result := s.db.WithContext(ctx).Where(accountAndIDQueryCondition, accountId, groupID).First(&group)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return status.Errorf(status.NotFound, "group not found for account")
		}
		return status.Errorf(status.Internal, "issue finding group: %s", result.Error)
	}

	for _, existingPeerID := range group.Peers {
		if existingPeerID == peerId {
			return nil
		}
	}

	group.Peers = append(group.Peers, peerId)

	if err := s.db.Save(&group).Error; err != nil {
		return status.Errorf(status.Internal, "issue updating group: %s", err)
	}

	return nil
}

// GetAccountPeers retrieves peers for an account.
func (s *SqlStore) GetAccountPeers(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbpeer.Peer, error) {
	var peers []*nbpeer.Peer
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Find(&peers, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get peers from store")
	}

	return peers, nil
}

// GetUserPeers retrieves peers for a user.
func (s *SqlStore) GetUserPeers(ctx context.Context, lockStrength LockingStrength, accountID, userID string) ([]*nbpeer.Peer, error) {
	var peers []*nbpeer.Peer
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Find(&peers, "account_id = ? AND user_id = ?", accountID, userID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get peers from store")
	}

	return peers, nil
}

// GetAccountPeersWithExpiration retrieves a list of peers that have login expiration enabled and added by a user.
func (s *SqlStore) GetAccountPeersWithExpiration(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbpeer.Peer, error) {
	var peers []*nbpeer.Peer
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Where("login_expiration_enabled = ? AND user_id IS NOT NULL AND user_id != ''", true).
		Find(&peers, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers with expiration from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get peers with expiration from store")
	}

	return peers, nil
}

// GetAccountPeersWithInactivity retrieves a list of peers that have login expiration enabled and added by a user.
func (s *SqlStore) GetAccountPeersWithInactivity(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbpeer.Peer, error) {
	var peers []*nbpeer.Peer
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Where("inactivity_expiration_enabled = ? AND user_id IS NOT NULL AND user_id != ''", true).
		Find(&peers, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers with inactivity from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get peers with inactivity from store")
	}

	return peers, nil
}

// GetPeerByID retrieves a peer by its ID and account ID.
func (s *SqlStore) GetPeerByID(ctx context.Context, lockStrength LockingStrength, accountID, peerID string) (*nbpeer.Peer, error) {
	var peer *nbpeer.Peer
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&peer, accountAndIDQueryCondition, accountID, peerID)
	if err := result.Error; err != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "peer not found")
		}
		log.WithContext(ctx).Errorf("failed to get peer from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get peer from store")
	}

	return peer, nil
}

// GetAllEphemeralPeers retrieves all peers with Ephemeral set to true across all accounts, optimized for batch processing.
func (s *SqlStore) GetAllEphemeralPeers(ctx context.Context, lockStrength LockingStrength) ([]*nbpeer.Peer, error) {
	var allEphemeralPeers, batchPeers []*nbpeer.Peer
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Where("ephemeral = ?", true).
		FindInBatches(&batchPeers, batchSize, func(tx *gorm.DB, batch int) error {
			allEphemeralPeers = append(allEphemeralPeers, batchPeers...)
			return nil
		})

	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to retrieve ephemeral peers: %s", result.Error)
		return nil, fmt.Errorf("failed to retrieve ephemeral peers")
	}

	return allEphemeralPeers, nil
}

func (s *SqlStore) DeletePeer(ctx context.Context, lockStrength LockingStrength, accountID string, peerID string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&nbpeer.Peer{}, accountAndIDQueryCondition, accountID, peerID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete peer from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete peer from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "peer not found")
	}

	return nil
}

func (s *SqlStore) AddPeerToAccount(ctx context.Context, peer *nbpeer.Peer) error {
	if err := s.db.WithContext(ctx).Create(peer).Error; err != nil {
		return status.Errorf(status.Internal, "issue adding peer to account: %s", err)
	}

	return nil
}

func (s *SqlStore) IncrementNetworkSerial(ctx context.Context, lockStrength LockingStrength, accountId string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		Where(idQueryCondition, accountId).Update("network_serial", gorm.Expr("network_serial + 1"))
	if result.Error != nil {
		return status.Errorf(status.Internal, "issue incrementing network serial count: %s", result.Error)
	}
	return nil
}

func (s *SqlStore) ExecuteInTransaction(ctx context.Context, operation func(store Store) error) error {
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	repo := s.withTx(tx)
	err := operation(repo)
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}

func (s *SqlStore) withTx(tx *gorm.DB) Store {
	return &SqlStore{
		db: tx,
	}
}

func (s *SqlStore) GetDB() *gorm.DB {
	return s.db
}

func (s *SqlStore) GetAccountDNSSettings(ctx context.Context, lockStrength LockingStrength, accountID string) (*DNSSettings, error) {
	var accountDNSSettings AccountDNSSettings

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		First(&accountDNSSettings, idQueryCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "dns settings not found")
		}
		return nil, status.Errorf(status.Internal, "failed to get dns settings from store: %v", result.Error)
	}
	return &accountDNSSettings.DNSSettings, nil
}

// SaveDNSSettings saves the DNS settings to the store.
func (s *SqlStore) SaveDNSSettings(ctx context.Context, lockStrength LockingStrength, accountID string, settings *DNSSettings) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		Where(idQueryCondition, accountID).Updates(&AccountDNSSettings{DNSSettings: *settings})
	if result.Error != nil {
		return status.Errorf(status.Internal, "failed to save dns settings to store: %v", result.Error)
	}
	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "account not found")
	}
	return nil
}

// AccountExists checks whether an account exists by the given ID.
func (s *SqlStore) AccountExists(ctx context.Context, lockStrength LockingStrength, id string) (bool, error) {
	var accountID string

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).
		Select("id").First(&accountID, idQueryCondition, id)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, result.Error
	}

	return accountID != "", nil
}

// GetAccountDomainAndCategory retrieves the Domain and DomainCategory fields for an account based on the given accountID.
func (s *SqlStore) GetAccountDomainAndCategory(ctx context.Context, lockStrength LockingStrength, accountID string) (string, string, error) {
	var account Account

	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&Account{}).Select("domain", "domain_category").
		Where(idQueryCondition, accountID).First(&account)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", "", status.Errorf(status.NotFound, "account not found")
		}
		return "", "", status.Errorf(status.Internal, "failed to get domain category from store: %v", result.Error)
	}

	return account.Domain, account.DomainCategory, nil
}

// GetGroupByID retrieves a group by ID and account ID.
func (s *SqlStore) GetGroupByID(ctx context.Context, lockStrength LockingStrength, accountID, groupID string) (*nbgroup.Group, error) {
	var group *nbgroup.Group
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&group, accountAndIDQueryCondition, accountID, groupID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "group not found")
		}
		log.WithContext(ctx).Errorf("failed to get group from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get group from store")
	}

	return group, nil
}

// GetGroupByName retrieves a group by name and account ID.
func (s *SqlStore) GetGroupByName(ctx context.Context, lockStrength LockingStrength, accountID, groupName string) (*nbgroup.Group, error) {
	var group nbgroup.Group

	// TODO: This fix is accepted for now, but if we need to handle this more frequently
	// we may need to reconsider changing the types.
	query := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Preload(clause.Associations)
	if s.storeEngine == PostgresStoreEngine {
		query = query.Order("json_array_length(peers::json) DESC")
	} else {
		query = query.Order("json_array_length(peers) DESC")
	}

	result := query.First(&group, "account_id = ? AND name = ?", accountID, groupName)
	if err := result.Error; err != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "group not found")
		}
		return nil, status.Errorf(status.Internal, "failed to get group from store: %s", result.Error)
	}
	return &group, nil
}

// SaveGroup saves a group to the store.
func (s *SqlStore) SaveGroup(ctx context.Context, lockStrength LockingStrength, group *nbgroup.Group) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(group)
	if result.Error != nil {
		return status.Errorf(status.Internal, "failed to save group to store: %v", result.Error)
	}
	return nil
}

// DeleteGroup deletes a group from the database.
func (s *SqlStore) DeleteGroup(ctx context.Context, lockStrength LockingStrength, accountID, groupID string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&nbgroup.Group{}, accountAndIDQueryCondition, accountID, groupID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete group from the store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to delete group from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "group not found")
	}

	return nil
}

// DeleteGroups deletes groups from the database.
func (s *SqlStore) DeleteGroups(ctx context.Context, strength LockingStrength, accountID string, groupIDs []string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(strength)}).
		Delete(&nbgroup.Group{}, " account_id = ? AND id IN ?", accountID, groupIDs)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to delete groups from the store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to delete groups from store: %v", result.Error)
	}

	return nil
}

// GetAccountPolicies retrieves policies for an account.
func (s *SqlStore) GetAccountPolicies(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*Policy, error) {
	var policies []*Policy
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Preload(clause.Associations).Find(&policies, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get policies from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get policies from store")
	}

	return policies, nil
}

// GetPolicyByID retrieves a policy by its ID and account ID.
func (s *SqlStore) GetPolicyByID(ctx context.Context, lockStrength LockingStrength, accountID, policyID string) (*Policy, error) {
	var policy *Policy
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Preload(clause.Associations).First(&policy, accountAndIDQueryCondition, accountID, policyID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "policy not found")
		}
		log.WithContext(ctx).Errorf("failed to get policy from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get policy from store")
	}

	return policy, nil
}

// SavePolicy saves a policy to the database.
func (s *SqlStore) SavePolicy(ctx context.Context, lockStrength LockingStrength, policy *Policy) error {
	result := s.db.WithContext(ctx).Session(&gorm.Session{FullSaveAssociations: true}).
		Clauses(clause.Locking{Strength: string(lockStrength)}).Save(policy)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save policy to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save policy to store")
	}
	return nil
}

func (s *SqlStore) DeletePolicy(ctx context.Context, lockStrength LockingStrength, accountID, policyID string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&Policy{}, accountAndIDQueryCondition, accountID, policyID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete policy from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete policy from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "policy not found")
	}

	return nil
}

// GetAccountPostureChecks retrieves posture checks for an account.
func (s *SqlStore) GetAccountPostureChecks(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*posture.Checks, error) {
	var postureChecks []*posture.Checks
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Find(&postureChecks, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get posture checks from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get posture checks from store")
	}

	return postureChecks, nil
}

// GetPostureChecksByID retrieves posture checks by their ID and account ID.
func (s *SqlStore) GetPostureChecksByID(ctx context.Context, lockStrength LockingStrength, accountID, postureCheckID string) (*posture.Checks, error) {
	var postureCheck *posture.Checks
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&postureCheck, accountAndIDQueryCondition, accountID, postureCheckID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "posture check not found")
		}
		log.WithContext(ctx).Errorf("failed to get posture check from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get posture check from store")
	}

	return postureCheck, nil
}

// SavePostureChecks saves a posture checks to the database.
func (s *SqlStore) SavePostureChecks(ctx context.Context, lockStrength LockingStrength, postureCheck *posture.Checks) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(postureCheck)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return status.Errorf(status.InvalidArgument, "name should be unique")
		}
		log.WithContext(ctx).Errorf("failed to save posture checks to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save posture checks to store")
	}

	return nil
}

// DeletePostureChecks deletes a posture checks from the database.
func (s *SqlStore) DeletePostureChecks(ctx context.Context, lockStrength LockingStrength, accountID, postureChecksID string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&posture.Checks{}, accountAndIDQueryCondition, accountID, postureChecksID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete posture checks from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete posture checks from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "posture checks not found")
	}

	return nil
}

// GetAccountRoutes retrieves network routes for an account.
func (s *SqlStore) GetAccountRoutes(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*route.Route, error) {
	var routes []*route.Route
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Find(&routes, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get routes from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get routes from store")
	}

	return routes, nil
}

// GetRouteByID retrieves a route by its ID and account ID.
func (s *SqlStore) GetRouteByID(ctx context.Context, lockStrength LockingStrength, accountID string, routeID string) (*route.Route, error) {
	var route *route.Route
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&route, accountAndIDQueryCondition, accountID, routeID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "route not found")
		}
		log.WithContext(ctx).Errorf("failed to get route from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get route from store")
	}

	return route, nil
}

// SaveRoute saves a route to the database.
func (s *SqlStore) SaveRoute(ctx context.Context, lockStrength LockingStrength, route *route.Route) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(route)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save route to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save route to store")
	}

	return nil
}

// DeleteRoute deletes a route from the database.
func (s *SqlStore) DeleteRoute(ctx context.Context, lockStrength LockingStrength, accountID, routeID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&route.Route{}, accountAndIDQueryCondition, accountID, routeID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete route from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete route from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "route not found")
	}

	return nil
}

// GetAccountSetupKeys retrieves setup keys for an account.
func (s *SqlStore) GetAccountSetupKeys(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*SetupKey, error) {
	var setupKeys []*SetupKey
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Find(&setupKeys, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get setup keys from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get setup keys from store")
	}

	return setupKeys, nil
}

// GetSetupKeyByID retrieves a setup key by its ID and account ID.
func (s *SqlStore) GetSetupKeyByID(ctx context.Context, lockStrength LockingStrength, accountID, setupKeyID string) (*SetupKey, error) {
	var setupKey *SetupKey
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&setupKey, accountAndIDQueryCondition, accountID, setupKeyID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "setup key not found")
		}
		log.WithContext(ctx).Errorf("failed to get setup key from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get setup key from store")
	}

	return setupKey, nil
}

// SaveSetupKey saves a setup key to the database.
func (s *SqlStore) SaveSetupKey(ctx context.Context, lockStrength LockingStrength, setupKey *SetupKey) error {
	result := s.db.WithContext(ctx).Session(&gorm.Session{FullSaveAssociations: true}).
		Clauses(clause.Locking{Strength: string(lockStrength)}).Save(setupKey)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save name server group to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save name server group to store")
	}

	return nil
}

func (s *SqlStore) DeleteSetupKey(ctx context.Context, lockStrength LockingStrength, accountID, keyID string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&SetupKey{}, accountAndIDQueryCondition, accountID, keyID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete setup key from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete setup key from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "setup key not found")
	}

	return nil
}

// GetAccountNameServerGroups retrieves name server groups for an account.
func (s *SqlStore) GetAccountNameServerGroups(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbdns.NameServerGroup, error) {
	var nsGroups []*nbdns.NameServerGroup
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Find(&nsGroups, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get name server groups from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get name server groups from store")
	}

	return nsGroups, nil
}

// GetNameServerGroupByID retrieves a name server group by its ID and account ID.
func (s *SqlStore) GetNameServerGroupByID(ctx context.Context, lockStrength LockingStrength, accountID, nsGroupID string) (*nbdns.NameServerGroup, error) {
	var nsGroup *nbdns.NameServerGroup
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&nsGroup, accountAndIDQueryCondition, accountID, nsGroupID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "name server group not found")
		}
		log.WithContext(ctx).Errorf("failed to get name server group from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get name server group from store")
	}

	return nsGroup, nil
}

// SaveNameServerGroup saves a name server group to the database.
func (s *SqlStore) SaveNameServerGroup(ctx context.Context, lockStrength LockingStrength, nameServerGroup *nbdns.NameServerGroup) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(nameServerGroup)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save name server group to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save name server group to store")
	}
	return nil
}

// DeleteNameServerGroup deletes a name server group from the database.
func (s *SqlStore) DeleteNameServerGroup(ctx context.Context, lockStrength LockingStrength, accountID, nameServerGroupID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Delete(&nbdns.NameServerGroup{}, accountAndIDQueryCondition, accountID, nameServerGroupID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete name server group from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete name server group from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "name server group not found")
	}

	return nil
}

// GetPATByHashedToken returns a PersonalAccessToken by its hashed token.
func (s *SqlStore) GetPATByHashedToken(ctx context.Context, lockStrength LockingStrength, hashedToken string) (*PersonalAccessToken, error) {
	var pat PersonalAccessToken
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).First(&pat, "hashed_token = ?", hashedToken)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPATNotFoundError()
		}
		log.WithContext(ctx).Errorf("failed to get pat from the store: %s", result.Error)
		return nil, status.NewGetPATFromStoreError()
	}

	return &pat, nil
}

// GetPATByID retrieves a personal access token by its ID and user ID.
func (s *SqlStore) GetPATByID(ctx context.Context, lockStrength LockingStrength, userID string, patID string) (*PersonalAccessToken, error) {
	var pat PersonalAccessToken
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&pat, "id = ? AND user_id = ?", patID, userID)
	if err := result.Error; err != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPATNotFoundError()
		}
		log.WithContext(ctx).Errorf("failed to get PAT from the store: %s", err)
		return nil, status.NewGetPATFromStoreError()
	}

	return &pat, nil
}

// GetUserPATs retrieves personal access tokens for a user.
func (s *SqlStore) GetUserPATs(ctx context.Context, lockStrength LockingStrength, userID string) ([]*PersonalAccessToken, error) {
	var pats []*PersonalAccessToken
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Find(&pats, "user_id = ?", userID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get user PAT's from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get user PAT's from store")
	}

	return pats, nil
}

// MarkPATUsed marks a personal access token as used.
func (s *SqlStore) MarkPATUsed(ctx context.Context, lockStrength LockingStrength, patID string) error {
	patCopy := PersonalAccessToken{
		LastUsed: time.Now().UTC(),
	}

	fieldsToUpdate := []string{"last_used"}
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Select(fieldsToUpdate).Where(idQueryCondition, patID).Updates(&patCopy)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to mark PAT as used: %s", result.Error)
		return status.Errorf(status.Internal, "failed to mark PAT as used")
	}

	if result.RowsAffected == 0 {
		return status.NewPATNotFoundError()
	}

	return nil
}

// SavePAT saves a personal access token to the database.
func (s *SqlStore) SavePAT(ctx context.Context, lockStrength LockingStrength, pat *PersonalAccessToken) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).Save(pat)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save PAT to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save PAT to store")
	}

	return nil
}

// DeletePAT deletes a personal access token from the database.
func (s *SqlStore) DeletePAT(ctx context.Context, lockStrength LockingStrength, userID, patID string) error {
	result := s.db.WithContext(ctx).Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&PersonalAccessToken{}, "user_id = ? AND id = ?", userID, patID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete PAT from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete PAT from store")
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "PAT not found")
	}

	return nil
}

package account

import (
	"strings"

	"github.com/thehappieco/mailie/internal/provider"
)

// iCloud Mail is IMAP and SMTP on Apple's servers with an app-specific
// password, and it is stored as exactly that: an account of kind imap. The
// provider column's CHECK names only gmail, microsoft and imap, and widening
// it means rebuilding the accounts table, whose DROP cascades into every
// credential, folder and message that references it.
//
// So what makes an account iCloud is where it points, and this file is the
// only place that decides it. Apple offers no OAuth to arbitrary apps — its
// servers advertise XOAUTH2, but only for its partner programme — so an
// app-specific password is the only way in, for anyone.

// ICloud is the provider an iCloud account is presented as.
const ICloud = "icloud"

// Apple's documented servers (support.apple.com/102525). The IMAP username
// is "usually" the name part of the address, but SMTP needs the whole
// address, and one login_user serves both: it stays the address. An iCloud+
// custom-domain address is refused as a sign-in on both servers, so for one
// of those the login is the account's own iCloud address instead.
const (
	icloudIMAPHost = "imap.mail.me.com"
	icloudIMAPPort = 993
	icloudSMTPHost = "smtp.mail.me.com"
	icloudSMTPPort = 587
	icloudSMTPTLS  = "starttls"
)

// IsICloud reports whether an account is iCloud Mail: generic IMAP on
// Apple's server.
func (a Account) IsICloud() bool {
	return a.Provider == provider.KindIMAP && strings.EqualFold(a.IMAPHost, icloudIMAPHost)
}

// ProviderName is the provider an account is presented as: its kind, or
// icloud for generic IMAP on Apple's servers.
func (a Account) ProviderName() string {
	if a.IsICloud() {
		return ICloud
	}
	return string(a.Provider)
}

// IsICloudAddress reports whether an address is one only iCloud Mail serves.
// An iCloud+ custom domain is iCloud too, but nothing in the address says so:
// that account is iCloud because the person chose it.
func IsICloudAddress(email string) bool {
	// The last @: a quoted local part may carry one of its own.
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(email[at+1:])) {
	case "icloud.com", "me.com", "mac.com":
		return true
	}
	return false
}

// NamesServers reports whether a request says where the mail servers are,
// which an iCloud request may not: pointed anywhere but Apple, the account
// would be presented as something it is not.
func (req AddRequest) NamesServers() bool {
	return strings.TrimSpace(req.IMAPHost) != "" || req.IMAPPort != 0 ||
		strings.TrimSpace(req.SMTPHost) != "" || req.SMTPPort != 0 || req.SMTPTLS != ""
}

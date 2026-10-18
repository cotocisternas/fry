// Package db embeds the authoritative SQL schema used by sqlc and installation.
package db

import _ "embed"

//go:embed schema/receiptstore.sql
var receiptStoreSchema string

//go:embed schema/admin.sql
var adminSchema string

//go:embed schema/maildirectory.sql
var mailDirectorySchema string

// ReceiptStoreSchema returns the receipt storage component of the schema.
func ReceiptStoreSchema() string { return receiptStoreSchema }

// AdminSchema returns the administration component of the schema.
func AdminSchema() string { return adminSchema }

// MailDirectorySchema returns the mail directory component of the schema.
func MailDirectorySchema() string { return mailDirectorySchema }

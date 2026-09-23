package db

// Uploaded images live in workspace_assets (migration 00069), and the URL that names one
// lives elsewhere: server_settings.logo_url and banner_url for the workspace's own
// images, users.avatar_url for a member's. The two can disagree. Every instance that
// stored its images on disk before 00069 has URLs whose files are gone (on the fleet
// they were on an emptyDir, and nothing imported them), an export taken before 00069
// replays URLs with no rows behind them, and a row can be deleted without its URL by
// anything that is not the upload routes.
//
// ⛔ A URL with no image behind it is worse than no URL. A non-empty logo_url switches
// the booking emails into the tenant-branded layout and puts an <img> in every public
// page header, so a dead one renders as a broken image where the District default
// would otherwise have been. So the URL is READ through these expressions, which answer
// the empty string for one of this server's own serve paths whose row does not exist,
// and pass anything else through unchanged.
//
// Read-time rather than a one-off migration that clears the columns: it destroys
// nothing (reverting the code brings every URL back), it heals itself on the next
// upload, and it also covers the cases a migration run once cannot see — an import of
// an old export, a restored backup, a row removed later. The price is one primary-key
// probe per row read.
//
// The workspace_id join makes each expression correct on the platform handle too, where
// no policy scopes the subquery (the credential lookups in auth.go read users there).
//
// They are constants, not functions of an alias, so that a query built from them is
// itself a constant and nothing about it depends on a value at run time. The price is a
// fixed spelling at the call site, stated on each.

// LogoURLSQL is a SELECT expression for server_settings.logo_url that reads as the empty
// string when the URL is this server's /branding/ serve path and the workspace has no
// logo stored. For a query in which server_settings is not aliased.
const LogoURLSQL = `CASE WHEN server_settings.logo_url LIKE '/branding/%' AND NOT EXISTS (` +
	`SELECT 1 FROM workspace_assets wa WHERE wa.workspace_id = server_settings.workspace_id` +
	` AND wa.kind = 'logo' AND wa.owner_id = '') ` +
	`THEN '' ELSE COALESCE(server_settings.logo_url, '') END`

// BannerURLSQL is LogoURLSQL for server_settings.banner_url and the stored banner.
const BannerURLSQL = `CASE WHEN server_settings.banner_url LIKE '/branding/%' AND NOT EXISTS (` +
	`SELECT 1 FROM workspace_assets wa WHERE wa.workspace_id = server_settings.workspace_id` +
	` AND wa.kind = 'banner' AND wa.owner_id = '') ` +
	`THEN '' ELSE COALESCE(server_settings.banner_url, '') END`

// AvatarURLSQL is a SELECT expression for users.avatar_url that reads as the empty
// string when the URL is this server's /avatars/ serve path and the user has no avatar
// stored. For a query in which users is aliased u.
const AvatarURLSQL = `CASE WHEN u.avatar_url LIKE '/avatars/%' AND NOT EXISTS (` +
	`SELECT 1 FROM workspace_assets wa WHERE wa.workspace_id = u.workspace_id` +
	` AND wa.kind = 'avatar' AND wa.owner_id = u.id) ` +
	`THEN '' ELSE COALESCE(u.avatar_url, '') END`

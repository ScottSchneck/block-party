# Changelog

## 1.7.0

- **A Mods tab for Fabric, Forge and NeoForge servers.** It works like the
  Plugins tab: search Modrinth (and CurseForge, with a key) for mods made
  for the server's loader and Minecraft version that run on servers,
  install one with the mods it needs, see when there's an update, turn
  mods off and on, and remove them. Each download is checked against the
  site's checksum. Mods added by hand show too.
- **Updates for add-ons from CurseForge.** A Bedrock add-on installed from
  CurseForge remembers where it came from; the Add-ons tab shows **Update
  available** when there's a newer file for the server's Minecraft version,
  and **Update** installs it over the old one.
- **Modpack updates.** A server made from a Modrinth or CurseForge modpack
  shows when the pack has a newer version, on its Mods tab. **Update
  modpack** backs up the whole server first, then puts in the new version's
  Minecraft, loader, mods and settings. Worlds, files changed by hand and
  mods added yourself are kept; the old version's files the new one doesn't
  have are removed. This works for servers made from now on (the panel
  needs to know which files the pack put in).
- **Consoles can join NeoForge servers.** NeoForge servers get Geyser and
  Floodgate for NeoForge, like Fabric servers get theirs, and a new one
  goes on the newest Minecraft Geyser supports that has a finished
  NeoForge. NeoForge servers made before get console support at their next
  Update (turn it off under Settings → Console players if you'd rather
  not). Forge still can't: Geyser doesn't run on it.
- **Lobbies.** A new Java server type, Lobby (Velocity), puts one address
  in front of several Paper and Purpur servers: friends land on the first
  and move between them with `/server`. Pick its servers on its Settings
  tab. The panel sets up both sides (the lobby's forwarding secret, and
  each server's Velocity support with its account checks left to the
  lobby), and console players join through the lobby with Geyser for
  Velocity.
- **"Updates are out" notifications.** Once a day the panel checks each
  server's plugins, mods, add-ons from CurseForge and modpack for new
  versions, and sends one notification per server about the ones it
  hasn't mentioned yet (turn it on for any channel under Settings →
  Notifications). Discord messages are now coloured cards, red for crashes
  and failed backups, amber for join requests, green for players, blue for
  updates.

## 1.6.1

- **CurseForge searches only show what will work.** For a Bedrock server,
  add-ons made for its Minecraft version or an older one; the panel
  installs the file made for the server's version when a creator posts
  one per version. For modpacks, ones for Fabric, Forge or NeoForge (not
  Quilt, which the panel doesn't run). Ones the creator only allows
  downloading from CurseForge itself are left out too. Each search says
  how many it hid and why.
- **Add-ons that need a newer Minecraft aren't installed.** An add-on's
  manifest says the oldest Minecraft it runs on; when that's newer than
  the server's, the panel says so instead of installing something the
  server would quietly leave out. This applies to uploaded add-ons too.
- Modrinth modpack search leaves out packs that are only for Quilt.

## 1.6.0

- **CurseForge.** Add a CurseForge API key (free, from
  console.curseforge.com) on the Settings page, and:
  - each Bedrock server's Add-ons tab gets **Find add-ons on CurseForge**:
    search add-ons, texture packs and worlds, and install them with one
    button (add-ons go into the world being played, worlds are added to
    the list);
  - Add server → Java → Modpack can search CurseForge as well as Modrinth.
    The panel installs the pack's Minecraft version and mod loader, then
    its server pack when it has one, or its mods (leaving out ones only for
    players' games, texture packs and shaders) and settings.

  Some creators only allow downloads from CurseForge itself; the panel
  respects that and says where to get those. The key is checked with
  CurseForge, kept in its own file on the server, and never shown again.

## 1.5.1

- **Plugin search only shows plugins that will install.** Hangar and
  Modrinth match a plugin if any of its versions is for the server's
  Minecraft; the panel now checks each result has a download for this
  version and hides the rest (it says how many). Plugins on Modrinth made
  for Spigot or Bukkit, which Paper and Purpur run, now show and install
  too; before, only files marked for Paper counted.

## 1.5.0

- **Computer use graphs.** A server's Overview shows its processor and
  memory use and the players on it, over the last hour, day or week. Point
  at a graph (or tap it) to see the value at that time; "Show as a table"
  lists the now, average and highest. Readings are kept for a week.
- **Scheduled tasks** (a server's Tasks tab): restart every night, send a
  message every hour, run console commands, start, stop or back up. At a
  time on chosen days, every so often, on a cron schedule, or a while after
  another task (a chain, like a message and then a restart). Restarts and
  stops can warn players first, and any task can be skipped while someone
  is playing. Anyone who takes care of the server can set them up.
- **A file manager** (a server's Files tab): browse the server's folder,
  edit text files such as settings and plugin configs (it warns if the
  server changed a file since you opened it), upload by dropping files on
  the page, download, rename, make folders and delete. Anyone who takes
  care of the server can use it. Only the owner can change plugins, mods
  and the server's programs, or move or delete whole folders, and nothing
  outside the server's folder can be reached, even through links.
- **More Java server types and versions.** Add server → Java now offers
  Paper, Purpur, Vanilla, Fabric, Forge and NeoForge, on any Minecraft
  version each has (older ones get the Java they need, down to Java 8).
  Fabric servers can take console players too (Geyser and Floodgate for
  Fabric). A server on a chosen version, and every modded one, stays on its
  Minecraft version when updated.
- **Modpacks.** The owner can add a server from a Modrinth modpack, found
  by searching Modrinth or uploaded as a .mrpack file: the panel installs
  the Minecraft version, mod loader and server-side mods it needs, checking
  every download. (CurseForge packs aren't supported yet.)
- A Java player added while the server was starting (like you, on a new
  server) is no longer lost when Mojang's account service is slow: the
  change stays queued until the server confirms it, and is tried again
  every 2 minutes.
- Fixed the word "null" showing after the port at the top of a server's
  page when the server has no open hours.

## 1.4.3

- **A quieter log.** The details added while getting 1.26.60 consoles
  working (every connection and join request to the console server list,
  and how each menu loaded) now only show with `LOG_LEVEL=debug`. Failed
  joins and menus that didn't load still show.
- Removed a workaround for occasional stalled joins to the console server
  list; gophertunnel 1.63 fixed the cause.

## 1.4.2

- **The server list can verify Xbox sign-ins from 1.26.60 consoles
  again.** Consoles joining over NetherNet now sign their join themselves,
  so the list reported them as "not verified". It now checks the
  Microsoft-issued identity the console sends when it connects, and the
  join must come from the same console. `REQUIRE_SIGN_IN=true` works with
  1.26.60 consoles again. Each NetherNet join logs "NetherNet connection
  identity" with whether it was verified.

## 1.4.1

- **Consoles on Minecraft 1.26.60 can open the server list from LAN Games
  again.** They asked for a secure (HTTPS) join by the panel's IP address,
  refused its self-made certificate and stopped with error Terracotta. The
  list now turns secure joins away, so consoles use a plain join instead
  (they may ask once whether to trust the server). Set `SIGNALING=auto` for
  the old behaviour.
- **Consoles outside the house can join the server list over NetherNet.**
  The list only told them the home's internet address when `PUBLIC_IP` was
  set, so on 1.26.60 they were given the home address and couldn't connect.
  It now uses the address Friends outside the house finds (restart the
  container if that address changes).

## 1.4.0

- **The console server list works with Minecraft 1.26.60.** Consoles and
  phones on the new version were told the list was outdated. The list now
  speaks 1.26.60, and accepts 1.26.60 consoles joining over NetherNet by
  address (gophertunnel 1.63 and its newest fix, go-nethernet 1.0.26).
  Thanks to the Unraid forum for the heads-up.
- **Off-site backups** (Settings → Off-site backups): every new backup is
  also copied to Google Drive, iCloud Drive, OneDrive, Dropbox, Backblaze
  B2, Amazon S3 or a compatible service (Cloudflare R2, Wasabi, MinIO…), or
  another disk or share. The newest few of each server are kept there,
  optionally encrypted with your passphrase. Download any copy back to
  restore it, or to bring a server back after a new install. You're
  notified if copying fails.
- The container now includes rclone 1.75.2.

## 1.3.0

- **Play time** (Settings → Play time): how long each kid plays today and
  each day this week, across every server and under every name they play as.
  Optional daily limits (school days and weekends) and play hours (school
  nights and weekend nights), with warnings in the game 10, 5 and 1 minute
  before, then they're moved off the server. Give extra time for the day in
  one tap. A kid sees their own time left when they sign in.
- **Open hours for servers** (a server's Settings tab): warnings, then it
  saves and closes for the night, and opens again in the morning. Only the
  owner can start it in between, or keep it open late for one night.
- **Played this week** on each server's Players tab.

## 1.2.1

From [Revise0592's fork](https://github.com/Revise0592/block-party):

- **Backups of running Java servers on Paper for Minecraft 26 work again.**
  Paper 26 logs "System chat: Saved the game", which the panel didn't
  recognise, so every backup of a running Paper 26 server waited a minute and
  failed.
- **One spacing on every page**: the same gap between boxes and the same
  padding inside them, a little tighter on phones.

## 1.2.0

- **A Settings page** for the owner, in the menu: friends outside the house,
  people, overnight updates, notifications, importing servers and the setup
  guide. Help is now just help.
- **A notice when a newer Block Party is out**, on the Servers page and in
  Settings, with what's new and how to update. The panel checks GitHub's
  release list twice a day and downloads nothing (`PANEL_UPDATE_API=off`
  turns the check off).
- **A server's Settings tab** lines its two columns up at the top, and every
  setting's control is the same height (Safari drew menus shorter).
  Read-only values show as On and Off, and NetherNet and RakNet by name.

## 1.1.1

- Java servers on Paper for Minecraft 26 show who's playing again: it logs
  joins and leaves as "System chat: Steve joined the game". From
  [Revise0592's fork](https://github.com/Revise0592/block-party).

## 1.1.0

Ready for Minecraft 26.60 (October 27, 2026), which joins servers over
NetherNet instead of RakNet.

- **Consoles on Java servers**: Geyser now takes NetherNet as well as RakNet,
  on the server's port + 1 (TCP and UDP). The console server list sends
  NetherNet players there, and the invite tells friends which port to type.
  Restart Java servers with console players to pick it up.
- **Bedrock servers** get a fixed block of 20 UDP ports for NetherNet
  (from 49152), so they can be forwarded for friends outside, and tell
  players outside the home's internet address.
- **The console server list** takes NetherNet as well as RakNet
  (`CONNECTION` now defaults to `both`), on UDP 19300-19319.
- **Friends outside the house** forwards everything NetherNet needs, and
  lists the forwards to add by hand, with ranges kept to one line.

## 1.0.0

The first release of Block Party: one container that runs a family's game
servers at home, with a web panel for desktop and phone and a console server
list so Xbox, PlayStation and Switch players can join.

### Games

- **Minecraft Bedrock**: the panel downloads Mojang's official server, gives
  each server its own port, and keeps it updated (backing up first).
- **Minecraft Java (Paper)**, with the Java it needs, and plugins from Hangar
  and Modrinth (installed by the owner).
- **Consoles on Java servers** through Geyser and Floodgate, with ViaVersion,
  so Bedrock players and consoles can join Java worlds. A server with console
  players stays on the newest Minecraft version Geyser supports.
- **Hytale**, signed in with your Hytale account, with a console, the
  allowlist and operators, settings and backups.

### Running servers

- Start, stop, restart and update, a live console, automatic restart after a
  crash, and servers that were running come back after a reboot.
- Players: who's online, the allowlist, operators, and **Tried to join** with
  an **Allow** button.
- Settings as labelled controls with a line of explanation each, plus an
  advanced editor.
- Worlds and add-ons: install `.mcaddon` and `.mcpack` files, upload, switch
  and download worlds.
- Scheduled backups with one-click restore and download, taken without kicking
  anyone off.
- Overnight updates and restarts, only when nobody is playing.
- Import from Crafty Controller, which is only read, never changed.

### Joining

- A console server list and built-in DNS (replacing BedrockConnect): consoles
  see your servers under **LAN Games**, or in a menu after joining a featured
  server.
- **Friends outside the house**: open a server to friends, with UPnP or the
  port forwards to add by hand, and an invite that explains how to join.

### People and safety

- An owner account, plus accounts for the kids with what each one can do on
  each server. Only the owner can delete servers or manage accounts.
- Optional two-factor sign-in with an authenticator app and recovery codes.
- Notifications to a phone (ntfy), Discord, email or text message.
- Activity history for each server.

### The panel

- Two looks, **Control Room** and **Treehouse**, each light or dark, saved
  with your account. Pages use the whole window on wide screens and work on
  phones.
- A setup guide the first time, and a Help page.

### Installing

- An Unraid Community Apps template. The container needs its own IP on your
  home network (Network Type br0, fixed IP).
- `LIST_IP` is optional: the panel uses the container's own address.
- The container starts as root only to give a brand-new data folder to
  `nobody:users` (or `PUID`/`PGID`), then runs as that user.

### Upgrading from Blockheads Control Panel

The project was renamed. Use the image `ghcr.io/scottschneck/block-party` and
the container name `blockparty`; existing data folders work as they are.

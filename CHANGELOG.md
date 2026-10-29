# Changelog

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

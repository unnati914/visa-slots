# Free hosting on Oracle Cloud (Always Free)

## 1. Account (once, ~10 min)
1. Go to https://signup.cloud.oracle.com
2. **Home Region: India West (Mumbai)** or India South (Hyderabad). This can't be changed later.
3. A card is needed for verification only. Always Free resources are never charged.

## 2. Create the server (~5 min)
Menu → Compute → Instances → **Create instance**
- **Name:** visa-slots
- **Image:** click *Edit* → *Change image* → **Canonical Ubuntu 24.04**
- **Shape:** *Change shape* → **Ampere** → VM.Standard.A1.Flex → 2 OCPU, 12 GB (Always Free eligible)
  - If you see "Out of capacity", choose **AMD → VM.Standard.E2.1.Micro** instead (also free)
- **Networking:** keep the defaults, and make sure "Assign a public IPv4 address" is **on**
- **SSH keys:** choose **Paste public key** and paste the contents of `deploy/ssh-key.pub`
- Click **Create**. After ~1 min, copy the **Public IP address**.

## 3. Open the web ports (once)
On the instance page: click the **Subnet** link → **Security** (or Security Lists) → Default Security List
→ **Add Ingress Rules**
- Source CIDR: `0.0.0.0/0` · IP Protocol: TCP · Destination port range: `80,443`

## 4. Deploy (from your Mac)
```
./deploy/deploy.sh <public-ip> <telegram-bot-token>
```
It prints the live link, e.g. `https://140-238-1-2.sslip.io`. Run the same command again to ship updates.
The token is optional and is remembered after the first time.

Logs:    `ssh -i ~/.ssh/visa_slots_oracle ubuntu@<ip> journalctl -u visa-slots -f`

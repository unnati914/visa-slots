package main

import "strings"

// Post is a US embassy or consulate that interviews visa applicants.
type Post struct {
	CC      string // ISO country code
	Country string
	City    string
}

// Visa types and appointment categories the agent tracks.
var VisaTypes = []string{"B1/B2", "F1/M1", "J1", "H1B", "L1", "O1", "C1/D"}
var Categories = []string{"Regular", "Dropbox"}

// US visa-issuing posts worldwide, one line per country: CC|Country|City,City,...
const postData = `
AF|Afghanistan|Kabul
AL|Albania|Tirana
DZ|Algeria|Algiers
AO|Angola|Luanda
AR|Argentina|Buenos Aires
AM|Armenia|Yerevan
AU|Australia|Sydney,Melbourne,Perth
AT|Austria|Vienna
AZ|Azerbaijan|Baku
BS|Bahamas|Nassau
BH|Bahrain|Manama
BD|Bangladesh|Dhaka
BB|Barbados|Bridgetown
BE|Belgium|Brussels
BZ|Belize|Belmopan
BJ|Benin|Cotonou
BO|Bolivia|La Paz
BA|Bosnia and Herzegovina|Sarajevo
BW|Botswana|Gaborone
BR|Brazil|Brasilia,Sao Paulo,Rio de Janeiro,Recife,Porto Alegre
BN|Brunei|Bandar Seri Begawan
BG|Bulgaria|Sofia
BF|Burkina Faso|Ouagadougou
BI|Burundi|Bujumbura
KH|Cambodia|Phnom Penh
CM|Cameroon|Yaounde
CA|Canada|Ottawa,Toronto,Vancouver,Montreal,Calgary,Halifax,Quebec City
CV|Cabo Verde|Praia
CF|Central African Republic|Bangui
TD|Chad|N'Djamena
CL|Chile|Santiago
CN|China|Beijing,Shanghai,Guangzhou,Shenyang,Wuhan
CO|Colombia|Bogota
CD|Congo (Kinshasa)|Kinshasa
CG|Congo (Brazzaville)|Brazzaville
CR|Costa Rica|San Jose
CI|Cote d'Ivoire|Abidjan
HR|Croatia|Zagreb
CU|Cuba|Havana
CY|Cyprus|Nicosia
CZ|Czech Republic|Prague
DK|Denmark|Copenhagen
DJ|Djibouti|Djibouti
DO|Dominican Republic|Santo Domingo
EC|Ecuador|Quito,Guayaquil
EG|Egypt|Cairo
SV|El Salvador|San Salvador
GQ|Equatorial Guinea|Malabo
ER|Eritrea|Asmara
EE|Estonia|Tallinn
SZ|Eswatini|Mbabane
ET|Ethiopia|Addis Ababa
FJ|Fiji|Suva
FI|Finland|Helsinki
FR|France|Paris
GA|Gabon|Libreville
GM|Gambia|Banjul
GE|Georgia|Tbilisi
DE|Germany|Berlin,Frankfurt,Munich
GH|Ghana|Accra
GR|Greece|Athens
GT|Guatemala|Guatemala City
GN|Guinea|Conakry
GY|Guyana|Georgetown
HT|Haiti|Port-au-Prince
HN|Honduras|Tegucigalpa
HK|Hong Kong|Hong Kong
HU|Hungary|Budapest
IS|Iceland|Reykjavik
IN|India|New Delhi,Mumbai,Chennai,Hyderabad,Kolkata
ID|Indonesia|Jakarta,Surabaya
IQ|Iraq|Baghdad,Erbil
IE|Ireland|Dublin
IL|Israel|Jerusalem,Tel Aviv
IT|Italy|Rome,Milan,Naples,Florence
JM|Jamaica|Kingston
JP|Japan|Tokyo,Osaka,Naha
JO|Jordan|Amman
KZ|Kazakhstan|Astana,Almaty
KE|Kenya|Nairobi
XK|Kosovo|Pristina
KW|Kuwait|Kuwait City
KG|Kyrgyzstan|Bishkek
LA|Laos|Vientiane
LV|Latvia|Riga
LB|Lebanon|Beirut
LS|Lesotho|Maseru
LR|Liberia|Monrovia
LT|Lithuania|Vilnius
LU|Luxembourg|Luxembourg
MG|Madagascar|Antananarivo
MW|Malawi|Lilongwe
MY|Malaysia|Kuala Lumpur
ML|Mali|Bamako
MT|Malta|Valletta
MR|Mauritania|Nouakchott
MU|Mauritius|Port Louis
MX|Mexico|Mexico City,Guadalajara,Monterrey,Tijuana,Ciudad Juarez,Hermosillo,Matamoros,Merida,Nogales,Nuevo Laredo
MD|Moldova|Chisinau
MN|Mongolia|Ulaanbaatar
ME|Montenegro|Podgorica
MA|Morocco|Rabat,Casablanca
MZ|Mozambique|Maputo
NA|Namibia|Windhoek
NP|Nepal|Kathmandu
NL|Netherlands|Amsterdam
NZ|New Zealand|Auckland
NI|Nicaragua|Managua
NE|Niger|Niamey
NG|Nigeria|Abuja,Lagos
MK|North Macedonia|Skopje
NO|Norway|Oslo
OM|Oman|Muscat
PK|Pakistan|Islamabad,Karachi
PA|Panama|Panama City
PG|Papua New Guinea|Port Moresby
PY|Paraguay|Asuncion
PE|Peru|Lima
PH|Philippines|Manila
PL|Poland|Warsaw,Krakow
PT|Portugal|Lisbon,Ponta Delgada
QA|Qatar|Doha
RO|Romania|Bucharest
RW|Rwanda|Kigali
SA|Saudi Arabia|Riyadh,Jeddah,Dhahran
SN|Senegal|Dakar
RS|Serbia|Belgrade
SL|Sierra Leone|Freetown
SG|Singapore|Singapore
SK|Slovakia|Bratislava
SI|Slovenia|Ljubljana
ZA|South Africa|Johannesburg,Cape Town,Durban
KR|South Korea|Seoul
SS|South Sudan|Juba
ES|Spain|Madrid,Barcelona
LK|Sri Lanka|Colombo
SR|Suriname|Paramaribo
SE|Sweden|Stockholm
CH|Switzerland|Bern
TW|Taiwan|Taipei
TJ|Tajikistan|Dushanbe
TZ|Tanzania|Dar es Salaam
TH|Thailand|Bangkok,Chiang Mai
TG|Togo|Lome
TT|Trinidad and Tobago|Port of Spain
TN|Tunisia|Tunis
TR|Turkey|Ankara,Istanbul
TM|Turkmenistan|Ashgabat
UG|Uganda|Kampala
UA|Ukraine|Kyiv
AE|United Arab Emirates|Abu Dhabi,Dubai
GB|United Kingdom|London,Belfast
UY|Uruguay|Montevideo
UZ|Uzbekistan|Tashkent
VN|Vietnam|Hanoi,Ho Chi Minh City
ZM|Zambia|Lusaka
ZW|Zimbabwe|Harare
`

// AllPosts returns every post in the list above.
func AllPosts() []Post {
	var out []Post
	for _, line := range strings.Split(strings.TrimSpace(postData), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		for _, city := range strings.Split(parts[2], ",") {
			out = append(out, Post{CC: parts[0], Country: parts[1], City: strings.TrimSpace(city)})
		}
	}
	return out
}

// Hot countries get more volatile mock data (slots open and vanish quickly).
var hotCountries = map[string]bool{"IN": true, "MX": true, "CN": true, "BR": true, "CO": true, "NG": true, "PH": true, "CA": true}

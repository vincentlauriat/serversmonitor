# ServersMonitor — Architecture

Miroir français de `ARCHITECTURE_EN.md`, qui est la source de vérité. Les deux fichiers se modifient
dans le même commit.

État : lots 1 et 2 livrés. Les lots 3 à 6 (Azure) sont de la roadmap, rien ci-dessous ne les décrit.

## L'invariant unique

**Le silence ne vaut jamais zéro.**

Une métrique non collectée vaut `nil` dans le protocole, `NULL` dans SQLite, `null` dans l'API, un
tiret dans le tableau et un trou dans le graphe. Jamais `0`. Un hôte qui cesse d'émettre est jugé par
la règle hors ligne seule, jamais par des seuils sur lesquels il ne peut plus rien dire.

Ce n'est pas une préférence de style. Un agent mort ressemble exactement à une machine dont le CPU
vient de tomber à 0 %, et toutes ses alertes se résoudraient d'elles-mêmes au pire moment. Chaque
couche porte cette règle : c'est pourquoi les champs optionnels d'un échantillon sont des pointeurs,
pourquoi l'agrégation propage les nulls, et pourquoi l'évaluateur ignore un hôte dont le dernier
échantillon a plus de deux intervalles.

## Forme

```
┌──────────────┐   WebSocket sortant, jeton porteur par hôte   ┌───────────────────────────┐
│   smagent    │ ───────────────────────────────────────────►  │           smhub           │
│  (un par     │       proto.Sample toutes les N secondes      │                           │
│   machine)   │ ◄───────────────────────────────────────────  │  ingest → store           │
└──────────────┘       proto.Welcome / Reconfigure             │      ↓                    │
                                                               │   alerts (machine d'état) │
                                                               │      ↓                    │
                                                               │   notify → deliveries     │
                                                               │      ↓                    │
                                 navigateur ◄── SSE + REST ──  │   server (+ web embarqué) │
                                                               └───────────┬───────────────┘
                                                                           │
                                                          SMTP · webhook/ntfy · Teams
```

Un processus pour le hub, un par machine surveillée, un fichier SQLite. Pas de broker de messages,
pas de base temporelle séparée, pas de reverse proxy à lui.

### Pourquoi l'agent appelle vers l'extérieur

C'est l'agent qui ouvre la connexion, pas le hub. Un Raspberry derrière une box, un portable sur le
réseau d'un café et une VM Azure sans adresse publique fonctionnent tous sans redirection de port ni
règle de pare-feu entrante. Le prix à payer : le hub ne peut pas joindre un agent qui ne s'est pas
manifesté, et c'est pour cela que c'est le hub, et non l'agent, qui décide qu'un hôte est hors ligne.

## Paquets

| Paquet | Responsabilité |
|---|---|
| `internal/proto` | Les messages JSON sur le fil. Aucune logique au-delà de l'encodage et du décodage. |
| `internal/agent/collect` | Lit la machine : cpu, mémoire, disque, réseau, charge, températures, Docker. |
| `internal/agent/link` | Le WebSocket sortant, la reconnexion, et le tampon qui couvre une coupure brève. |
| `internal/hub/store` | La base SQLite : schéma, migrations et toutes les requêtes. Aucun autre paquet n'écrit de SQL. |
| `internal/hub/ingest` | Authentifie un agent, lit ses échantillons, les écrit via le store. |
| `internal/hub/alerts` | La machine d'état et l'évaluateur. Pur : ni base, ni horloge propre. |
| `internal/hub/notify` | Le rendu des messages, les trois canaux, et le dispatcher. |
| `internal/hub/auth` | Hachage de mot de passe en Argon2id et limiteur de tentatives de connexion. |
| `internal/hub/server` | L'API REST, les Server-Sent Events, et le front embarqué. |
| `internal/hub/config` | Les variables d'environnement. Lues une fois au démarrage. |
| `internal/hub` | Le câblage et les tâches périodiques. Le seul paquet qui connaît tous les autres. |
| `web/` | SvelteKit 5, export statique, embarqué dans le binaire à la construction. |

### Sens des dépendances

`hub` dépend de tout ; rien ne dépend de `hub`. `server` n'importe pas `hub` : il déclare une
interface `Notifier` que `hub` satisfait, et c'est ce qui empêche le cycle de se former. `alerts` et
`notify` importent `store` pour ses types mais ne touchent jamais à une base à eux, donc les deux se
testent sans base.

## Le protocole

Quatre types de messages, une constante de version, `internal/proto`.

| Message | Sens | Rôle |
|---|---|---|
| `hello` | agent → hub | Premier message : version d'agent, OS, architecture, nom d'hôte, cœurs, mémoire totale. |
| `welcome` | hub → agent | Intervalle d'échantillonnage et points de montage à ignorer. |
| `sample` | agent → hub | Un instantané, à chaque intervalle. |
| `reconfigure` | hub → agent | Un nouvel intervalle, sans reconnexion. |

**Tous les champs optionnels d'un `Sample` sont des pointeurs.** `CPU *float64`, pas `float64`. Un
pointeur nil signifie que l'agent ne l'a pas collecté. Le décodage ignore les champs inconnus et
laisse les champs absents à nil, si bien qu'un agent plus ancien parlant à un hub plus récent se
dégrade au lieu d'échouer.

L'authentification est un jeton porteur par hôte, généré par le hub et affiché une seule fois. Le
store n'en garde que le SHA-256. Régénérer un jeton invalide l'ancien immédiatement.

## L'agent

`collect` lit à travers une interface, donc les tests ne touchent jamais la vraie machine.
`gopsutil.go` est le seul fichier qui appelle `gopsutil` ; `docker.go` parle HTTP directement sur la
socket unix Docker, deux endpoints, plutôt que d'embarquer le SDK Docker.

Deux choses apprises des données réelles, pas des tests :

- **Le bruit des montages.** Ce Mac remonte huit points de montage, dont sept sont des doublons du
  même conteneur APFS ou des volumes système que personne ne regarde. Une liste de préfixes plus une
  déduplication par `(used, total)` ramène à un. La liste est envoyée par le hub dans `welcome`, donc
  elle peut changer sans redéployer les agents.
- **Le nombre de capteurs.** Le même Mac remonte trente-neuf capteurs de température. `sensors()` les
  classe par pic et la page d'hôte trace les six premiers, en le disant.

`link` se reconnecte avec temporisation croissante et garde les derniers échantillons non envoyés,
du plus ancien au plus récent, pour qu'une coupure brève laisse un trou pas plus long qu'elle.

## Le store

SQLite via `modernc.org/sqlite`, en Go pur, donc le binaire se construit avec `CGO_ENABLED=0` et
l'image Docker se termine sur `scratch`.

Les migrations sont des fichiers SQL numérotés, embarqués par `go:embed`, appliqués dans l'ordre à
l'intérieur d'une transaction, avec la version dans une table `schema_version`. Les horodatages sont
stockés en chaînes RFC 3339 UTC, donc l'ordre lexical est l'ordre chronologique.

### Tables

| Table | Contient |
|---|---|
| `hosts` | Une ligne par machine : nom, empreinte du jeton, statut, dernière vue, champs du hello, indicateur de mise en sourdine. |
| `samples` | Les échantillons bruts. |
| `samples_10m`, `samples_1h`, `samples_1d` | Les moyennes agrégées. |
| `containers`, `container_samples` | Les conteneurs Docker et leurs séries. |
| `alert_rules` | Métrique, seuil, durée, hôte optionnel. |
| `alert_events` | Journal en ajout seul des transitions `fired` et `resolved`. |
| `deliveries` | Une ligne par (événement, canal) : `pending`, `sent` ou `failed`. |
| `users`, `sessions` | Le compte administrateur unique et ses cookies. |
| `settings` | Clé/valeur : intervalle, rétentions, et toute la configuration des notifications. |

Toutes les tables de séries cascadent depuis `hosts`, et `deliveries` cascade depuis `alert_events`.
Supprimer un hôte efface tout ce qui le concerne en une seule instruction.

**Le DDL de `samples_1h` et `samples_1d` est écrit en entier plutôt que dérivé par
`CREATE TABLE ... AS SELECT`.** Cette forme perd toutes les contraintes, clé étrangère comprise : la
cascade aurait donc silencieusement cessé de fonctionner et un hôte supprimé aurait laissé des lignes
derrière lui.

### Agrégation et rétention

Une tâche horaire agrège les échantillons bruts en fenêtres de 10 minutes, d'une heure et d'un jour.
Seules les fenêtres complètes sont écrites, et réécrire une fenêtre donne le même résultat : la tâche
est donc sûre à lancer n'importe quand, y compris deux fois, y compris après un crash. La rétention
est configurable par résolution ; les moyennes journalières sont gardées indéfiniment. Les livraisons
réglées sont purgées après 30 jours ; une livraison `pending` ne l'est jamais, parce que c'est du
travail qui reste à faire.

## Alertes

`alerts` est pur. `Evaluate` reçoit les hôtes, le dernier échantillon de chacun, les règles et
l'heure courante, et renvoie les transitions à persister. Il ne possède ni horloge ni base, donc ses
tests posent une situation et vérifient un résultat.

Les règles sont par métrique, avec un hôte optionnel. `applicable` garde exactement une règle par
métrique pour un hôte, en préférant la règle spécifique à la règle globale. Une seconde règle globale
pour la même métrique est ignorée.

La **règle hors ligne est implicite** et n'a pas de ligne : un hôte est hors ligne après trois
intervalles manqués, et cette décision appartient au hub. Elle porte l'identifiant de règle `0`.

Seules les transitions `pending → firing` et `firing → ok` sont enregistrées. Une métrique qui reste
au-dessus de son seuil produit un événement, pas un par évaluation.

Un hôte en sourdine est ignoré entièrement. Une règle supprimée pendant qu'elle est déclenchée laisse
une ligne `fired` que plus rien ne résoudra : c'est correct pour un journal en ajout seul et faux
pour un badge, donc le compteur d'alertes actives filtre les deux cas à l'affichage plutôt que
d'écrire un `resolved` de synthèse. Le journal reste honnête.

## Notifications

`notify` rend un `Message` une fois et le confie à chaque `Channel` activé, une interface à une seule
méthode.

**La ligne de livraison est écrite avant la première tentative.** C'est ce qui permet à un hub tué en
plein retry de rejouer le travail au démarrage suivant au lieu de le perdre en silence. La
conséquence est assumée et affichée dans l'interface : **la livraison est au moins une fois**. Un
crash entre le 200 du canal et l'enregistrement répète le message. Un doublon est une gêne ; une
alerte perdue est l'échec que tout ceci existe pour éviter.

Le dispatcher est un worker unique sur une file tenue comme un slice derrière un mutex, et non un
canal bufferisé, parce que la politique d'éviction doit inspecter ce qui est déjà en file.

- **Retries** à 1 s, 5 s, 25 s, quatre tentatives. Les `429` et les `5xx` reviennent ; les autres
  `4xx` non, parce qu'ils échoueront identiquement pour toujours. Le nombre de tentatives enregistré
  est celui qui a réellement servi, pas un nombre déduit de la classe de la dernière erreur.
- **`Enqueue` ne bloque jamais.** Il s'exécute sur la goroutine du hub, et un webhook mort ne doit pas
  empêcher le hub d'évaluer ses alertes.
- **File pleine : on évince le plus ancien `fired`**, un `resolved` seulement quand il ne reste que
  ça. Perdre la résolution, c'est laisser tout le monde sur « cette machine est tombée ». Chaque
  éviction est enregistrée comme livraison échouée avec la raison `queue full`.

La configuration des canaux vit dans `settings`, pas dans l'environnement, donc elle est éditable
depuis le navigateur sans redémarrer. Le mot de passe SMTP ne sort jamais de l'API : en lecture, seul
`smtp_password_set` est exposé ; en écriture, un champ absent signifie « ne touche pas » et une
chaîne vide signifie « efface ».

**Teams n'a jamais livré vers un vrai tenant depuis ce code.** Le payload Adaptive Card est vérifié
octet par octet contre le contrat documenté par Microsoft et le comportement HTTP est testé, mais une
URL de workflow appartient à un tenant. Les URL de connecteur Office 365 ne fonctionnent plus depuis
mai 2026 et ne sont pas supportées.

## Le processus hub

`Run` démarre le dispatcher, rejoue les livraisons en attente, puis cadence :

| À chaque | Fait |
|---|---|
| intervalle d'échantillonnage | Marque les hôtes silencieux ; un passage hors ligne évalue tout de suite au lieu d'attendre. |
| minute | Évalue les règles, persiste les transitions, les publie, les confie au dispatcher. |
| heure | Agrège, purge les échantillons, les sessions et les livraisons réglées. |

La configuration des canaux est tenue dans un `atomic.Pointer` : le handler des réglages remplace le
tout, l'évaluateur ne fait jamais que lire.

## La couche web

REST sous `/api/v1`, session par cookie, mot de passe en Argon2id, limiteur sur la connexion. Les
mises à jour en direct sont des Server-Sent Events sur `/api/v1/events`, unidirectionnels et donc
triviaux à faire passer par un proxy, plutôt qu'un second WebSocket.

Le front est SvelteKit 5 avec `adapter-static`, construit en fichiers statiques et embarqué par
`go:embed` depuis `internal/hub/webdist/build`, jamais depuis `web/`, pour que `go vet ./...` ne
traverse pas `node_modules`. Les graphes sont en `uplot`. Un seul binaire sert l'API et l'interface.

## Configuration

Hub, lu une fois au démarrage :

| Variable | Défaut | Sens |
|---|---|---|
| `SM_LISTEN` | `:8090` | Adresse d'écoute. |
| `SM_DATA_DIR` | `./data` | Où vit le fichier SQLite. |
| `SM_SECURE_COOKIES` | `false` | Pose l'attribut Secure ; à activer derrière du TLS. |
| `SM_LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error`. |

Agent, en options ou en environnement :

| Variable | Option | Sens |
|---|---|---|
| `SM_HUB` | `--hub` | URL du hub, `wss://hub.example`. |
| `SM_TOKEN` | `--token` | Le jeton de l'hôte. |
| `SM_INSECURE` | `--insecure` | Autorise un `ws://` en clair vers un hôte distant. |
| `SM_DOCKER_SOCKET` | `--docker-socket` | Par défaut `/var/run/docker.sock` ; vide désactive Docker. |

Tout le reste — intervalle d'échantillonnage, rétentions, règles d'alerte, canaux de notification —
est dans la base et éditable depuis l'interface.

## Ce qui est délibérément absent

- **Pas de TLS propre.** Un reverse proxy le fait mieux. `SM_SECURE_COOKIES=true` derrière lui.
- **Pas de comptes utilisateurs.** Un seul administrateur local. Entra ID est repoussé.
- **Pas de routage par règle.** Chaque canal activé reçoit chaque transition. Mettre un hôte en
  sourdine pour le faire taire.
- **Pas de cluster.** Un hub, un fichier SQLite, une machine.

## Tests

178 tests Go sur 12 paquets et 26 tests front, plus un test bout en bout qui lance un vrai hub et un
vrai agent sur un vrai WebSocket et vérifie qu'une alerte atteint un webhook et que la livraison est
enregistrée.

Deux habitudes méritent d'être gardées, parce que toutes deux ont attrapé des défauts que la suite
n'a pas vus :

- **Vérifier à la main contre un hub qui tourne.** Six défauts du lot 1 et deux du lot 2 viennent de
  contrôles manuels : le code de sortie sur un port occupé, le bruit des montages, une légende de
  capteurs plus haute que son graphe, des libellés d'axe tronqués, un chemin de socket unix
  dépassant la limite de 104 octets de macOS, des alertes fantômes, et une notification de test qui
  pointait vers un hôte inexistant.
- **Muter le code pour vérifier le test.** Un test de réglages passait contre un handler qui écrivait
  avant de valider, parce que l'ordre d'itération d'une map laissait un cas ultérieur réécrire la clé
  vérifiée. Un test d'éclatement par canal manquait entièrement, si bien qu'un `break` à la place du
  `continue` passait 174 tests.

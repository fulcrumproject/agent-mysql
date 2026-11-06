# Agent MySQL v1

## Perimetro

- Agent Go basato su `agent-lib-go/stdagent`, con configurazione dal core e adapter Docker.
- Un agent locale per host Linux, con socket Unix Docker. Nessun servizio HTTP intermedio.
- Un container MySQL Community 9.7 LTS e un volume dati persistente per servizio. Immagine ufficiale fissata a patch e digest, non selezionabile dal cliente.
- Database, username e password forniti alla creazione; password nel vault persistente del core. Utente con privilegi sul solo database assegnato, amministrazione riservata all'agent.
- CPU e RAM configurabili, disco senza quota. Sono limiti massimi, non prenotazioni di risorse.
- Hostname privato dell'host e porta stabile assegnata da un pool del core. Connessioni non TLS consentite; TLS non disabilitato forzatamente. Nessuna gestione dei certificati.

## Criteri

1. `create` termina con MySQL avviato solo dopo una connessione SQL riuscita con le credenziali del cliente.
2. `stop` conserva dati/configurazione e `start` li riutilizza.
3. `update` modifica solo CPU/RAM a servizio fermo; dati, credenziali ed endpoint invariati.
4. `delete`, solo a servizio fermo, elimina definitivamente le risorse possedute dal servizio, inclusi i dati.
5. Policy Docker `unless-stopped`: ripartenza automatica dopo crash/riavvio host, senza riavviare istanze esplicitamente fermate.
6. Operazioni ripetibili senza duplicazioni, senza adottare o modificare risorse estranee.
7. Errori e timeout conservano i dati; ripetizione esplicita, nessun nuovo sistema di retry dei job. Consentire di fermare e cancellare anche una creazione parziale.
8. Nessun secret nei log, nelle variabili visibili tramite Docker inspect o nei dati runtime restituiti al core.
9. Health dell'agent e metriche CPU/RAM/uptime/disponibilità MySQL; monitoraggio dello spazio libero sul filesystem Docker condiviso, non una quota del servizio.
10. Documentazione, esempi e definizioni Fulcrum, licenza Apache 2.0.

## Esclusioni

Backup/restore, HA, gestione remota/multi-host, quote disco, certificati gestiti, upgrade automatici, immagini alternative, rinomina database, cambio utente, rotazione password, ridimensionamento a caldo. Nessuna garanzia di trasporto cifrato o recupero dati.

## Verifica

Test unitari con mock; integrazione con Docker e MySQL reali su dati temporanei per persistenza, privilegi, limiti, errori, ownership e cancellazione; lifecycle completo attraverso un core di test. Non usare dati o infrastrutture di produzione.

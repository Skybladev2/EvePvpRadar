# Eve PvP Radar

A web application for finding PvP activity in EVE Online.

## Prerequsites

* Docker Compose
* Network access to the following sites:
  * https://r2z2.zkillboard.com
  * https://api.eve-scout.com
  * https://esi.evetech.net
  * https://login.eveonline.com

## Run

1. Configure `.env` file (see `.env.example`). For single-domain hosting, leave `API_URL` empty — nginx proxies all requests to the backend, which serves the app and API.
2. Build service:
```
IMAGE_TAG=$(git rev-parse HEAD) docker compose build
```
3. Run the service
```
docker compose up
```
4. Stop the service
```
docker compose down -v
```

## Debugging

For clean build:
```
IMAGE_TAG=$(git rev-parse HEAD) docker compose build --no-cache && docker compose up --force-recreate
```

## Performance optimization

This application was optimized using techniques from this video: https://www.youtube.com/watch?v=-Ln-8QM8KhQ

## Legal

See [CCP.md](CCP.md) for the CCP copyright notice.

EVE SSO login button images are provided by [CCP's SSO Developer Documentation](https://developers.eveonline.com/docs/services/sso) and are the property of CCP hf.

`standing-human.svg` is based on "population" by Eliricon from [Noun Project](https://thenounproject.com/browse/icons/term/population/) (CC BY 3.0)

`running-human.svg` is based on "Running Man" by Aha-Soft from [Noun Project](https://thenounproject.com/browse/icons/term/running-man/) (CC BY 3.0)

`filter.svg` is based on "filter" by scott desmond from [Noun Project](https://thenounproject.com/browse/icons/term/filter/) (CC BY 3.0)

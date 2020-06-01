#!/bin/sh

set +e

gcloud --project [GOOGLE_PROJECT] alpha functions deploy [FUNCTION_NAME] --verbosity debug \
  --entry-point F \
  --memory 128MB \
  --region asia-east2 \
  --runtime go113 \
  --env-vars-file .env.staging.yaml \
  --trigger-http \
  --allow-unauthenticated
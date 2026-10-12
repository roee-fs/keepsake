package controllers

// Version is this release. scripts/release.sh sets it to Chart.yaml's appVersion.
const Version = "0.6.0"

// Actor is the revision log's actor in auth mode none, where a request carries no caller identity.
const Actor = "keepsake/" + Version

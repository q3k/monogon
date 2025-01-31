// This is a 'Jenkinsfile'-style declarative 'Pipeline' definition. It is
// executed by Jenkins for presubmit checks, ie. checks that run against an
// open Gerrit change request.

// TODO(leo): remove once CI image has been updated.
def gazelle_build = "curl -o ~/bazelisk https://storage.googleapis.com/monogon-infra-public/bazelisk-v1.15.0 && chmod +x ~/bazelisk"

pipeline {
    agent none
    options {
        disableConcurrentBuilds()
    }
    stages {
        stage('Parallel') {
            parallel {
                stage('Test') {
                    agent {
                        node {
                            label ""
                            customWorkspace '/home/ci/monogon'
                        }
                    }
                    steps {
                        gerritCheck checks: ['jenkins:test': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                        echo "Gerrit change: ${GERRIT_CHANGE_URL}"
                        sh "git clean -fdx -e '/bazel-*'"
                        sh gazelle_build
                        sh "JENKINS_NODE_COOKIE=dontKillMe ~/bazelisk test //..."
                        sh "JENKINS_NODE_COOKIE=dontKillMe ~/bazelisk test -c dbg //..."
                    }
                    post {
                        success {
                            gerritCheck checks: ['jenkins:test': 'SUCCESSFUL']
                        }
                        unsuccessful {
                            gerritCheck checks: ['jenkins:test': 'FAILED']
                        }
                    }
                }

                stage('Gazelle') {
                    agent {
                        node {
                            label ""
                            customWorkspace '/home/ci/monogon'
                        }
                    }
                    steps {
                        gerritCheck checks: ['jenkins:gazelle': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                        echo "Gerrit change: ${GERRIT_CHANGE_URL}"
                        sh "git clean -fdx -e '/bazel-*'"
                        sh gazelle_build
                        sh "JENKINS_NODE_COOKIE=dontKillMe ~/bazelisk run //:gazelle-update-repos"
                        sh "JENKINS_NODE_COOKIE=dontKillMe ~/bazelisk run //:gazelle -- update"
                        sh "JENKINS_NODE_COOKIE=dontKillMe ~/bazelisk run //:go -- mod tidy"

                        script {
                            def diff = sh script: "git status --porcelain", returnStdout: true
                            if (diff.trim() != "") {
                                sh "git diff HEAD"
                                error """
                                    Unclean working directory after running gazelle.
                                    Please run:

                                       \$ bazel run //:gazelle-update-repos
                                       \$ bazel run //:gazelle -- update

                                    In your git checkout and amend the resulting diff to this changelist.
                                """
                            }
                        }
                    }
                    post {
                        success {
                            gerritCheck checks: ['jenkins:gazelle': 'SUCCESSFUL']
                        }
                        unsuccessful {
                            gerritCheck checks: ['jenkins:gazelle': 'FAILED']
                        }
                    }
                }
            }

            post {
                success {
                    gerritReview labels: [Verified: 1]
                }
                unsuccessful {
                    gerritReview labels: [Verified: -1]
                }
            }
        }
    }
}

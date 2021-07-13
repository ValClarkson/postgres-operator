prerequisite: operator 4.x is running
prerequisite: cluster named hippo exists via the 4.x operator


Install litmus, follow the directions located here
https://docs.litmuschaos.io/docs/getstarted/

Notes:  the current version of litmuschaos docs is missing the annotation
section. You will need to add an annotation to the clusters that will
be tested via litmuschaos
https://docs.litmuschaos.io/docs/1.13.5/getstarted/#annotate-your-application

kubectl annotate deployment.apps/hippo litmuschaos.io/chaos="true" -n pgo
//TODO operator tests


after installation of litmus chaos these general tests are available
k get chaosexperiments.litmuschaos.io -n pgo
NAME                      AGE
container-kill            3d19h
disk-fill                 3d19h
disk-loss                 3d19h
docker-service-kill       3d19h
k8-pod-delete             3d19h
k8-service-kill           3d19h
kubelet-service-kill      3d19h
node-cpu-hog              3d19h
node-drain                3d19h
node-io-stress            3d19h
node-memory-hog           3d19h
node-poweroff             3d19h
node-restart              3d19h
node-taint                3d19h
pod-autoscaler            3d19h
pod-cpu-hog               3d19h
pod-delete                3d19h
pod-dns-error             3d19h
pod-dns-spoof             3d19h
pod-io-stress             3d19h
pod-memory-hog            3d19h
pod-network-corruption    3d19h
pod-network-duplication   3d19h
pod-network-latency       3d19h
pod-network-loss          3d19h


To view results
   k describe chaosresult hippo-deletepod-chaos-pod-delete -n pgo




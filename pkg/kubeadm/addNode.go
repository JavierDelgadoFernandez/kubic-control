// Copyright 2019, 2020 Thorsten Kukuk
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubeadm

import (
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	pb "github.com/thkukuk/kubic-control/api"
	"github.com/thkukuk/kubic-control/pkg/tools"
)

var (
	joincmd_g         = ""
	token_create_time time.Time
)

func AddNode(in *pb.AddNodeRequest, stream pb.Kubeadm_AddNodeServer) error {
	// XXX Check if node isn't already part of the kubernetes cluster

	haproxy_salt := ""
	nodeNames := in.NodeNames
	nodeType := in.Type
	master_salt := Read_Cfg("control-plane.conf", "master")

	// If the join command is older than 23 hours, generate a new one. Else re-use the old one.
	if time.Since(token_create_time).Hours() > 23 {
		stream.Send(&pb.StatusReply{Success: true, Message: "Generate new token ..."})
		log.Info("Token to join nodes too old, creating new one")

		success, token := executeCmdSalt(master_salt, "kubeadm", "token", "create", "--print-join-command")
		if success != true {
			if err := stream.Send(&pb.StatusReply{Success: false, Message: token}); err != nil {
				return err
			}
			return nil
		}
		if len(master_salt) > 0 {
			token = strings.Replace(token, "\n", "", -1)
			i := strings.Index(token, ":") + 1
			token = strings.TrimSpace(token[i:])
		}
		joincmd_g = strings.TrimSuffix(token, "\n")
		token_create_time = time.Now()
	}

	joincmd := joincmd_g

	// if nodeType is not set, assume worker
	if len(nodeType) == 0 {
		nodeType = "worker"
	}

	if strings.EqualFold(nodeType, "master") {
		joincmd = joincmd + " --control-plane"

		stream.Send(&pb.StatusReply{Success: true, Message: "Upload certificates ..."})
		success, lines := executeCmdSalt(master_salt, "kubeadm", "init", "phase", "upload-certs", "--upload-certs")
		if success != true {
			if err := stream.Send(&pb.StatusReply{Success: false, Message: lines}); err != nil {
				return err
			}
			return nil
		}
		// the key is the third line in the output
		cert_key := strings.Split(strings.Replace(lines, ":", "", -1), "\n")
		joincmd = joincmd + " --certificate-key " + strings.TrimSuffix(string(cert_key[2]), "\n")
		haproxy_salt = Read_Cfg("control-plane.conf", "loadbalancer_salt")
	}

	// Ping all nodes to get an exact list of node names
	var success bool
	var message string
	var nodelist []string

	// Differentiate between 'name1,name2' and 'name[1,2]'
	if strings.Index(nodeNames, ",") >= 0 && strings.Index(nodeNames, "[") == -1 {
		success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", "--out=txt",
			"-L", nodeNames, "test.ping")
	} else {
		success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", "--out=txt",
			nodeNames, "test.ping")
	}
	if success != true {
		if err := stream.Send(&pb.StatusReply{Success: false, Message: message}); err != nil {
			return err
		}
		return nil
	}
	// we have a list of minions, only use the one where the line ends with "True"
	list := strings.Split(message, "\n")
	for _, entry := range list {
		if strings.HasSuffix(entry, ": True") {
			list := strings.Split(entry, ":")
			nodelist = append(nodelist, list[0])
		}
	}

	nodelistLength := len(nodelist)
	var wg sync.WaitGroup
	wg.Add(nodelistLength)

	failed := 0
	for i := 0; i < nodelistLength; i++ {
		go func(i int) {
			defer wg.Done()

			stream.Send(&pb.StatusReply{Success: true, Message: nodelist[i] + ": adding node..."})

			success, message := tools.ExecuteCmd("salt", "--module-executors='direct_call'", nodelist[i], "service.start", "crio")
			if success != true {
				if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
					log.Errorf("Send message failed: %s", err)
				}
				failed++
				return
			}
			success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", nodelist[i], "service.enable", "crio")
			if success != true {
				if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
					log.Errorf("Send message failed: %s", err)
				}
				failed++
				return
			}
			success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", nodelist[i], "service.start", "kubelet")
			if success != true {
				if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
					log.Errorf("Send message failed: %s", err)
				}
				failed++
				return
			}
			success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", nodelist[i], "service.enable", "kubelet")
			if success != true {
				if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
					log.Errorf("Send message failed: %s", err)
				}
				failed++
				return
			}

			stream.Send(&pb.StatusReply{Success: true, Message: nodelist[i] + ": joining cluster..."})

			success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", nodelist[i], "cmd.run", "\""+joincmd+"\"")
			if success != true {
				if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
					log.Errorf("Send message failed: %s", err)
				}
				failed++
				return
			}
			success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", nodelist[i], "grains.append", "kubicd", "kubic-"+nodeType+"-node")
			if success != true {
				if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
					log.Errorf("Send message failed: %s", err)
				}
				failed++
				return
			}
			// Configure transactinal-update
			success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", nodelist[i], "cmd.run", "if [ -f /etc/transactional-update.conf ]; then grep -q ^REBOOT_METHOD= /etc/transactional-update.conf && sed -i -e 's|REBOOT_METHOD=.*|REBOOT_METHOD=kured|g' /etc/transactional-update.conf || echo REBOOT_METHOD=kured >> /etc/transactional-update.conf ; else echo REBOOT_METHOD=kured > /etc/transactional-update.conf ; fi")
			if success != true {
				if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
					log.Errorf("Send message failed: %s", err)
				}
				failed++
				return
			}
			// If master and loadbalancer is known, add to haproxy
			if len(haproxy_salt) > 0 {
				stream.Send(&pb.StatusReply{Success: true, Message: nodelist[i] + ": adding node to haproxy loadbalancer..."})

				success, message = tools.ExecuteCmd("salt", "--module-executors='direct_call'", haproxy_salt, "cmd.run", "haproxycfg server add "+nodelist[i])
				if success != true {
					if err := stream.Send(&pb.StatusReply{Success: false, Message: nodelist[i] + ": " + message}); err != nil {
						log.Errorf("Send message failed: %s", err)
					}
					failed++
					return
				}
			}
			stream.Send(&pb.StatusReply{Success: true, Message: nodelist[i] + ": node successful added"})
		}(i)
	}

	wg.Wait()
	if failed > 0 {
		if err := stream.Send(&pb.StatusReply{Success: false, Message: "An error occured during adding Node(s)"}); err != nil {
			return err
		}
	}
	return nil
}

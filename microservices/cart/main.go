package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"

	"github.com/golang/protobuf/ptypes/empty"
	pb "github.com/kubernetes-native-testbed/kubernetes-native-testbed/microservices/cart/protobuf"
	orderpb "github.com/kubernetes-native-testbed/kubernetes-native-testbed/microservices/order/protobuf"
	productpb "github.com/kubernetes-native-testbed/kubernetes-native-testbed/microservices/product/protobuf"
	"google.golang.org/grpc"
	health "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

var (
	kvsHost string
	kvsPort int

	orderHost string
	orderPort int

	productHost string
	productPort int
)

const (
	defaultBindAddr = ":8080"

	componentName  = "cart"
	defaultKVSHost = "cart-db-pd.cart.svc.cluster.local"
	defaultKVSPort = 2379

	defaultOrderHost = "order.order.svc.cluster.local"
	defaultOrderPort = 8080

	defaultProductHost = "product.product.svc.cluster.local"
	defaultProductPort = 8080
)

func init() {
	var err error
	if kvsHost = os.Getenv("KVS_HOST"); kvsHost == "" {
		kvsHost = defaultKVSHost
	}
	if kvsPort, err = strconv.Atoi(os.Getenv("KVS_PORT")); err != nil {
		kvsPort = defaultKVSPort
		log.Printf("kvsPort parse error: %v", err)
	}
}

type cartAPIServer struct {
	cartRepository  cartRepository
	orderClient     orderpb.OrderAPIClient
	orderEndpoint   string
	productClient   productpb.ProductAPIClient
	productEndpoint string
}

func (s *cartAPIServer) Show(ctx context.Context, req *pb.ShowRequest) (*pb.ShowResponse, error) {
	userUUID := req.GetUserUUID()
	cart, notfound, err := s.cartRepository.findByUUID(userUUID)
	if err != nil {
		return nil, err
	}
	if notfound {
		return nil, fmt.Errorf("cart is not found for %s", userUUID)
	}
	log.Printf("show %s", cart)
	return &pb.ShowResponse{Cart: convertToCartProto(cart)}, nil
}

func (s *cartAPIServer) Add(ctx context.Context, req *pb.AddRequest) (*empty.Empty, error) {
	additionalCart := convertToCart(req.GetCart())
	log.Printf("add %s", additionalCart)
	cart, notfound, err := s.cartRepository.findByUUID(additionalCart.UserUUID)
	if err != nil {
		return nil, err
	}
	if notfound {
		log.Printf("store cart for new record")
		if _, err := s.cartRepository.store(additionalCart); err != nil {
			return nil, err
		}
		return &empty.Empty{}, nil
	}

	log.Printf("base cart %s", cart)
	for productUUID, increaseCount := range additionalCart.CartProducts {
		if _, ok := cart.CartProducts[productUUID]; ok {
			// increase
			cart.CartProducts[productUUID] += increaseCount
		} else {
			// add
			cart.CartProducts[productUUID] = increaseCount
		}
	}

	log.Printf("update cart: %s", cart)
	if err := s.cartRepository.update(cart); err != nil {
		return nil, err
	}

	return &empty.Empty{}, nil
}

func (s *cartAPIServer) Remove(ctx context.Context, req *pb.RemoveRequest) (*empty.Empty, error) {
	additionalCart := convertToCart(req.GetCart())
	log.Printf("remove %s", additionalCart)
	cart, notfound, err := s.cartRepository.findByUUID(additionalCart.UserUUID)
	if err != nil {
		return nil, err
	}
	if notfound {
		return nil, fmt.Errorf("cart is not found for %s", additionalCart.UserUUID)
	}

	log.Printf("base cart %s", cart)
	for productUUID, decreaseCount := range additionalCart.CartProducts {
		if count, ok := cart.CartProducts[productUUID]; ok {
			// decrease and remove
			count -= decreaseCount
			if count <= 0 {
				delete(cart.CartProducts, productUUID)
			} else {
				cart.CartProducts[productUUID] = count
			}
		}
	}

	log.Printf("update cart: %s", cart)
	if err := s.cartRepository.update(cart); err != nil {
		return nil, err
	}

	return &empty.Empty{}, nil
}

func (s *cartAPIServer) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	retry := 1
	orderedProducts := make([]*orderpb.OrderedProduct, 0, len(req.GetCart().GetCartProducts()))

	cart := convertToCart(req.GetCart())
	for productUUID, count := range cart.CartProducts {
		var err error
		var productResp *productpb.GetResponse
		for i := 0; i < retry; i++ {
			productResp, err = s.productClient.Get(ctx, &productpb.GetRequest{UUID: productUUID})
			if err != nil {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		orderedProducts = append(orderedProducts, &orderpb.OrderedProduct{
			ProductUUID: productResp.GetProduct().GetUUID(),
			Count:       int32(count),
			Price:       int32(productResp.GetProduct().GetPrice()),
		})

	}

	orderReq := &orderpb.SetRequest{
		Order: &orderpb.Order{
			UserUUID:        cart.UserUUID,
			PaymentInfoUUID: req.GetPaymentInfoUUID(),
			AddressUUID:     req.GetAddressUUID(),
			OrderedProducts: orderedProducts,
		},
	}
	var err error
	var orderResp *orderpb.SetResponse
	for i := 0; i < retry; i++ {
		orderResp, err = s.orderClient.Set(ctx, orderReq)
		if err != nil {
			continue
		}
	}
	if err != nil {
		return nil, err
	}

	return &pb.CommitResponse{OrderUUID: orderResp.GetUUID()}, nil
}

func (s *cartAPIServer) recoverMicroserviceConnection(client interface{}) error {
	switch client.(type) {
	case orderpb.OrderAPIClient:
		conn, err := grpc.Dial(s.orderEndpoint, grpc.WithInsecure())
		if err != nil {
			return err
		}
		s.orderClient = orderpb.NewOrderAPIClient(conn)
	case productpb.ProductAPIClient:
		conn, err := grpc.Dial(s.productEndpoint, grpc.WithInsecure())
		if err != nil {
			return err
		}
		s.productClient = productpb.NewProductAPIClient(conn)
	}
	return nil
}

func main() {
	lis, err := net.Listen("tcp", defaultBindAddr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	log.Printf("listen on %s", defaultBindAddr)

	crConfig := cartRepositoryTiKVConfig{
		ctx:       context.Background(),
		pdAddress: kvsHost,
		pdPort:    kvsPort,
	}
	cr, closeCr, err := crConfig.connect()
	if err != nil {
		log.Fatalf("failed to connect to kvs: %v (config=%#v)", err, crConfig)
	}
	defer closeCr()
	log.Printf("successed to connect to kvs")

	orderConn, err := grpc.Dial(fmt.Sprintf("%s:%d", orderHost, orderPort), grpc.WithInsecure())
	if err != nil {
		log.Fatal(err)
	}
	orderClient := orderpb.NewOrderAPIClient(orderConn)
	productConn, err := grpc.Dial(fmt.Sprintf("%s:%d", productHost, productPort), grpc.WithInsecure())
	if err != nil {
		log.Fatal(err)
	}
	productClient := productpb.NewProductAPIClient(productConn)

	s := grpc.NewServer()
	api := &cartAPIServer{
		cartRepository: cr,
		orderClient:    orderClient,
		productClient:  productClient,
	}
	pb.RegisterCartAPIServer(s, api)

	healthpb.RegisterHealthServer(s, health.NewServer())

	log.Printf("start cart API server")
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
